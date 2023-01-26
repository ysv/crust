package blockexplorer

// Ports defines ports used by applications required to run block explorer.
type Ports struct {
	Postgres  int
	Hasura    int
	BDJuno    int
	BigDipper int
}
