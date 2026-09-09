package pricing

import (
	"embed"
	"io/fs"
)

//go:embed all:seed
var seedFS embed.FS

// SeedFS returns the price files shipped with the binary. They are embedded
// so a container starts with a usable table and no external mount.
func SeedFS() (fs.FS, string) { return seedFS, "seed" }

// LoadSeed builds a table from the embedded price files.
func LoadSeed() (*Table, error) {
	fsys, dir := SeedFS()
	return LoadFS(fsys, dir)
}
