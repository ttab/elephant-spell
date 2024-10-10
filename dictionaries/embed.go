package dictionaries

import "embed"

//go:embed *.*
var files embed.FS

func GetFS() embed.FS {
	return files
}
