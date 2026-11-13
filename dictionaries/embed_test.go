package dictionaries_test

import (
	"path"
	"testing"

	"github.com/ttab/elephant-spell/dictionaries"
)

// TestEmbeddedFiles checks that only hunspell dictionaries are embedded, and
// that every .dic file has the .aff file it is loaded with.
func TestEmbeddedFiles(t *testing.T) {
	fsys := dictionaries.GetFS()

	files, err := fsys.ReadDir(".")
	if err != nil {
		t.Fatalf("list embedded files: %v", err)
	}

	names := make(map[string]bool, len(files))

	for _, f := range files {
		names[f.Name()] = true
	}

	var dics int

	for name := range names {
		switch path.Ext(name) {
		case ".dic":
			dics++

			aff := name[:len(name)-len(".dic")] + ".aff"
			if !names[aff] {
				t.Errorf("%s is embedded without %s", name, aff)
			}
		case ".aff":
		default:
			t.Errorf("unexpected embedded file %q", name)
		}
	}

	if dics == 0 {
		t.Error("no dictionaries embedded")
	}
}
