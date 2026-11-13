package dictionaries

import "embed"

//go:embed *.aff *.dic
var files embed.FS

func GetFS() embed.FS {
	return files
}
