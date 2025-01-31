package coreum

import (
	"context"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/CoreumFoundation/coreum-tools/pkg/build"
	"github.com/CoreumFoundation/crust/build/golang"
)

// Test names.
const (
	TestIBC     = "ibc"
	TestModules = "modules"
	TestUpgrade = "upgrade"
)

// BuildAllIntegrationTests builds all the coreum integration tests.
func BuildAllIntegrationTests(ctx context.Context, deps build.DepsFunc) error {
	deps(ensureRepo)

	entries, err := os.ReadDir(testsDir)
	if err != nil {
		return errors.WithStack(err)
	}

	actions := make([]build.CommandFunc, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "contracts" {
			continue
		}

		actions = append(actions, BuildIntegrationTests(e.Name()))
	}
	deps(actions...)
	return nil
}

// BuildIntegrationTests returns function compiling integration tests.
func BuildIntegrationTests(name string) build.CommandFunc {
	return func(ctx context.Context, deps build.DepsFunc) error {
		prerequisites := []build.CommandFunc{golang.EnsureGo, ensureRepo}
		switch name {
		case TestModules:
			prerequisites = append(prerequisites, CompileModulesSmartContracts)
		case TestUpgrade:
			prerequisites = append(prerequisites, CompileModulesSmartContracts)
		case TestIBC:
			prerequisites = append(prerequisites, CompileIBCSmartContracts)
		}

		deps(prerequisites...)

		return golang.BuildTests(ctx, golang.TestBuildConfig{
			PackagePath:   filepath.Join(testsDir, name),
			BinOutputPath: filepath.Join(testsBinDir, repoName+"-"+name),
			Tags:          []string{"integrationtests"},
		})
	}
}
