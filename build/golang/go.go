package golang

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/CoreumFoundation/coreum-tools/pkg/build"
	"github.com/CoreumFoundation/coreum-tools/pkg/libexec"
	"github.com/CoreumFoundation/coreum-tools/pkg/logger"
	"github.com/CoreumFoundation/coreum-tools/pkg/must"
	"github.com/CoreumFoundation/crust/build/docker"
	"github.com/CoreumFoundation/crust/build/tools"
)

const goAlpineVersion = "3.18"

// BinaryBuildConfig is the configuration for `go build`.
type BinaryBuildConfig struct {
	// Platform is the platform to build the binary for
	Platform tools.Platform

	// PackagePath is the path to package to build
	PackagePath string

	// BinOutputPath is the path for compiled binary file
	BinOutputPath string

	// Tags is the list of additional tags to build
	Tags []string

	// LinkStatically triggers static compilation
	LinkStatically bool

	// CGOEnabled builds cgo binary
	CGOEnabled bool

	// Parameters is the set of values passed to -X flags of `go build`
	Parameters map[string]string
}

// TestBuildConfig is the configuration for `go test -c`.
type TestBuildConfig struct {
	// PackagePath is the path to package to build
	PackagePath string

	// BinOutputPath is the path for compiled binary file
	BinOutputPath string

	// Tags is the list of additional tags to build
	Tags []string
}

// EnsureGo ensures that go is available.
func EnsureGo(ctx context.Context, deps build.DepsFunc) error {
	return tools.EnsureTool(ctx, tools.Go)
}

// EnsureGolangCI ensures that go linter is available.
func EnsureGolangCI(ctx context.Context, deps build.DepsFunc) error {
	return tools.EnsureTool(ctx, tools.GolangCI)
}

// Build builds go binary.
func Build(ctx context.Context, config BinaryBuildConfig) error {
	if config.Platform.OS == tools.DockerOS {
		return buildInDocker(ctx, config)
	}
	return buildLocally(ctx, config)
}

func buildLocally(ctx context.Context, config BinaryBuildConfig) error {
	logger.Get(ctx).Info("Building go package locally", zap.String("package", config.PackagePath),
		zap.String("binary", config.BinOutputPath))

	if config.Platform != tools.PlatformLocal {
		return errors.Errorf("building requested for platform %s while only %s is supported",
			config.Platform, tools.PlatformLocal)
	}

	libDir := filepath.Join(tools.CacheDir(), "lib")
	if err := os.MkdirAll(libDir, 0o700); err != nil {
		return errors.WithStack(err)
	}
	args, envs, err := buildArgsAndEnvs(config, libDir)
	if err != nil {
		return err
	}
	args = append(args, "-o", must.String(filepath.Abs(config.BinOutputPath)), ".")
	envs = append(envs, os.Environ()...)

	cmd := exec.Command(tools.Path("bin/go", tools.PlatformLocal), args...)
	cmd.Dir = config.PackagePath
	cmd.Env = envs

	if err := libexec.Exec(ctx, cmd); err != nil {
		return errors.Wrapf(err, "building go package '%s' failed", config.PackagePath)
	}
	return nil
}

func buildInDocker(ctx context.Context, config BinaryBuildConfig) error {
	// FIXME (wojciech): use docker API instead of docker executable

	logger.Get(ctx).Info("Building go package in docker", zap.String("package", config.PackagePath),
		zap.String("binary", config.BinOutputPath))

	if _, err := exec.LookPath("docker"); err != nil {
		return errors.Wrap(err, "docker command is not available in PATH")
	}

	image, err := ensureBuildDockerImage(ctx)
	if err != nil {
		return err
	}

	srcDir := must.String(filepath.Abs(".."))
	goPath := GoPath()
	if err := os.MkdirAll(goPath, 0o700); err != nil {
		return errors.WithStack(err)
	}
	cacheDir := tools.BinariesRootPath(config.Platform)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return errors.WithStack(err)
	}
	workDir := filepath.Clean(filepath.Join("/src", "crust", config.PackagePath))
	nameSuffix := make([]byte, 4)
	must.Any(rand.Read(nameSuffix))

	args, envs, err := buildArgsAndEnvs(config, "/crust-cache/lib")
	if err != nil {
		return err
	}
	runArgs := []string{
		"run", "--rm",
		"--label", docker.LabelKey + "=" + docker.LabelValue,
		"-v", srcDir + ":/src",
		"-v", goPath + ":/go",
		"-v", cacheDir + ":/crust-cache",
		"--env", "GOPATH=/go",
		"--env", "GOCACHE=/crust-cache/go-build",
		"--workdir", workDir,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--name", "crust-build-" + filepath.Base(config.BinOutputPath) + "-" + hex.EncodeToString(nameSuffix),
	}
	if tools.PlatformLocal == tools.PlatformLinuxAMD64 && config.Platform == tools.PlatformDockerARM64 {
		crossCompilerPath := filepath.Dir(filepath.Dir(tools.Path("bin/aarch64-linux-musl-gcc", tools.PlatformDockerAMD64)))
		libWasmVMPath := tools.Path("lib/libwasmvm_muslc.a", tools.PlatformDockerARM64)
		runArgs = append(runArgs,
			"-v", crossCompilerPath+":/aarch64-linux-musl-cross",
			"-v", libWasmVMPath+":/aarch64-linux-musl-cross/aarch64-linux-musl/lib/libwasmvm_muslc.a",
		)
	}
	for _, env := range envs {
		runArgs = append(runArgs, "--env", env)
	}
	runArgs = append(runArgs, image)
	runArgs = append(runArgs, args...)
	runArgs = append(runArgs, "-o", "/src/crust/"+config.BinOutputPath, ".")
	if err := libexec.Exec(ctx, exec.Command("docker", runArgs...)); err != nil {
		return errors.Wrapf(err, "building package '%s' failed", config.PackagePath)
	}
	return nil
}

// BuildTests builds tests.
func BuildTests(ctx context.Context, config TestBuildConfig) error {
	logger.Get(ctx).Info("Building go tests", zap.String("package", config.PackagePath),
		zap.String("binary", config.BinOutputPath))

	args := []string{
		"test",
		"-c",
		"-o", must.String(filepath.Abs(config.BinOutputPath)),
	}
	if len(config.Tags) > 0 {
		args = append(args, "-tags="+strings.Join(config.Tags, ","))
	}

	cmd := exec.Command(tools.Path("bin/go", tools.PlatformLocal), args...)
	cmd.Dir = config.PackagePath

	if err := libexec.Exec(ctx, cmd); err != nil {
		return errors.Wrapf(err, "building go tests '%s' failed", config.PackagePath)
	}
	return nil
}

//go:embed Dockerfile.tmpl
var dockerfileTemplate string

var dockerfileTemplateParsed = template.Must(template.New("Dockerfile").Parse(dockerfileTemplate))

func ensureBuildDockerImage(ctx context.Context) (string, error) {
	dockerfileBuf := &bytes.Buffer{}
	err := dockerfileTemplateParsed.Execute(dockerfileBuf, struct {
		GOVersion     string
		AlpineVersion string
	}{
		GOVersion:     tools.ByName(tools.Go).Version,
		AlpineVersion: goAlpineVersion,
	})
	if err != nil {
		return "", errors.Wrap(err, "executing Dockerfile template failed")
	}
	dockerfileChecksum := sha256.Sum256(dockerfileBuf.Bytes())
	image := "crust-go-build:" + hex.EncodeToString(dockerfileChecksum[:4])

	imageBuf := &bytes.Buffer{}
	imageCmd := exec.Command("docker", "images", "-q", image)
	imageCmd.Stdout = imageBuf
	if err := libexec.Exec(ctx, imageCmd); err != nil {
		return "", errors.Wrapf(err, "failed to list image '%s'", image)
	}
	if imageBuf.Len() > 0 {
		return image, nil
	}

	buildCmd := exec.Command(
		"docker",
		"build",
		"--label", docker.LabelKey+"="+docker.LabelValue,
		"--tag", image,
		"--tag", "crust-go-build:latest",
		"-",
	)
	buildCmd.Stdin = dockerfileBuf

	if err := libexec.Exec(ctx, buildCmd); err != nil {
		return "", errors.Wrapf(err, "failed to build image '%s'", image)
	}
	return image, nil
}

func buildArgsAndEnvs(config BinaryBuildConfig, libDir string) (args, envs []string, err error) {
	var crossCompileARM64 bool
	switch config.Platform {
	case tools.PlatformLocal:
	case tools.PlatformDockerLocal:
	case tools.PlatformDockerARM64:
		if tools.PlatformLocal != tools.PlatformLinuxAMD64 {
			return nil, nil, errors.Errorf("crosscompiling for %s is possible only on platform %s", config.Platform, tools.PlatformLinuxAMD64)
		}
		crossCompileARM64 = true
	default:
		return nil, nil, errors.Errorf("building is not possible for platform %s on platform %s", config.Platform, tools.PlatformLocal)
	}

	ldFlags := []string{"-w", "-s"}
	if config.LinkStatically {
		ldFlags = append(ldFlags, "-extldflags=-static")
	}
	for k, v := range config.Parameters {
		ldFlags = append(ldFlags, "-X", k+"="+v)
	}
	args = []string{
		"build",
		"-trimpath",
		"-ldflags=" + strings.Join(ldFlags, " "),
	}
	if len(config.Tags) > 0 {
		args = append(args, "-tags="+strings.Join(config.Tags, ","))
	}

	envs = []string{
		"LIBRARY_PATH=" + libDir,
	}

	cgoEnabled := "0"
	if config.CGOEnabled {
		cgoEnabled = "1"
	}
	envs = append(envs, "CGO_ENABLED="+cgoEnabled)
	if crossCompileARM64 {
		envs = append(envs, "GOARCH=arm64", "CC=/aarch64-linux-musl-cross/bin/aarch64-linux-musl-gcc")
	}

	return args, envs, nil
}

// Generate calls `go generate` for specific package.
func Generate(ctx context.Context, path string, deps build.DepsFunc) error {
	deps(EnsureGo)
	log := logger.Get(ctx)
	log.Info("Running go generate", zap.String("path", path))

	cmd := exec.Command(tools.Path("bin/go", tools.PlatformLocal), "generate", "./...")
	cmd.Dir = path
	if err := libexec.Exec(ctx, cmd); err != nil {
		return errors.Wrapf(err, "generation failed in package '%s'", path)
	}
	return nil
}

// Test runs go tests in repository.
func Test(ctx context.Context, repoPath string, deps build.DepsFunc) error {
	deps(EnsureGo)
	log := logger.Get(ctx)
	return onModule(repoPath, func(path string) error {
		goCodePresent, err := containsGoCode(path)
		if err != nil {
			return err
		}
		if !goCodePresent {
			log.Info("No code to test", zap.String("path", path))
			return nil
		}

		log.Info("Running go tests", zap.String("path", path))
		cmd := exec.Command(tools.Path("bin/go", tools.PlatformLocal), "test", "-count=1", "-shuffle=on", "-race", "./...")
		cmd.Dir = path
		if err := libexec.Exec(ctx, cmd); err != nil {
			return errors.Wrapf(err, "unit tests failed in module '%s'", path)
		}
		return nil
	})
}

// Tidy runs go mod tidy in repository.
func Tidy(ctx context.Context, repoPath string, deps build.DepsFunc) error {
	deps(EnsureGo)
	log := logger.Get(ctx)
	return onModule(repoPath, func(path string) error {
		log.Info("Running go mod tidy", zap.String("path", path))

		cmd := exec.Command(tools.Path("bin/go", tools.PlatformLocal), "mod", "tidy")
		cmd.Dir = path
		if err := libexec.Exec(ctx, cmd); err != nil {
			return errors.Wrapf(err, "'go mod tidy' failed in module '%s'", path)
		}
		return nil
	})
}

func onModule(repoPath string, fn func(path string) error) error {
	return filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "go.mod" {
			return nil
		}
		return fn(filepath.Dir(path))
	})
}

func containsGoCode(path string) (bool, error) {
	errFound := errors.New("found")
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		return errFound
	})
	if errors.Is(err, errFound) {
		return true, nil
	}
	return false, errors.WithStack(err)
}

// GetModuleVersions returns a map[moduleName]version with version from go.mod for the specified module within the given repo.
func GetModuleVersions(deps build.DepsFunc, repoPath string, modules []string) (map[string]string, error) {
	moduleToVersion := make(map[string]string, len(modules))

	var version string
	var err error
	for _, module := range modules {
		version, err = getModuleVersion(deps, repoPath, module)
		if err != nil {
			return nil, err
		}

		moduleToVersion[module] = version
	}

	return moduleToVersion, nil
}

func getModuleVersion(deps build.DepsFunc, repoPath, moduleName string) (string, error) {
	deps(EnsureGo)

	args := []string{
		"list",
		"-m",
		moduleName,
	}

	cmd := exec.Command(tools.Path("bin/go", tools.PlatformLocal), args...)
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	parts := strings.Fields(string(output))
	if len(parts) < 2 {
		return "", errors.New("no module version")
	}

	return parts[1], nil
}

// GoPath returns $GOPATH.
func GoPath() string {
	goPath := os.Getenv("GOPATH")
	if goPath == "" {
		goPath = filepath.Join(must.String(os.UserHomeDir()), "go")
	}

	return goPath
}
