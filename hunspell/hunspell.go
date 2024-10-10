package hunspell

// #cgo linux LDFLAGS: -lhunspell
//
// #include <stdlib.h>
// #include <stdio.h>
// #include <hunspell/hunspell.h>
import "C"

import (
	"runtime"
	"sync"
	"unsafe"
)

type Checker struct {
	m      sync.Mutex
	handle *C.Hunhandle
}

func NewChecker(affixPath string, dictPath string) (*Checker, error) {
	c := Checker{}

	cAffPath := C.CString(affixPath)
	defer C.free(unsafe.Pointer(cAffPath))

	cDictPath := C.CString(dictPath)
	defer C.free(unsafe.Pointer(cDictPath))

	c.handle = C.Hunspell_create(cAffPath, cDictPath)

	runtime.SetFinalizer(&c, func(c *Checker) {
		C.Hunspell_destroy(c.handle)

		c.handle = nil
	})

	return &c, nil
}

func (c *Checker) Suggest(word string) []string {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	var (
		cArray **C.char
		length C.int
	)

	c.m.Lock()
	length = C.Hunspell_suggest(c.handle, &cArray, cWord)
	c.m.Unlock()

	defer C.Hunspell_free_list(c.handle, &cArray, length)

	words := goStringSlice(cArray, int(length))

	return words
}

func (c *Checker) Add(word string) bool {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	c.m.Lock()
	r := C.Hunspell_add(c.handle, cWord)
	c.m.Unlock()

	return int(r) == 0
}

func (c *Checker) Remove(word string) bool {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	c.m.Lock()
	r := C.Hunspell_remove(c.handle, cWord)
	c.m.Unlock()

	return int(r) == 0
}

func (c *Checker) Stem(word string) []string {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	var (
		carray **C.char
		length C.int
	)

	c.m.Lock()
	length = C.Hunspell_stem(c.handle, &carray, cWord)
	c.m.Unlock()

	defer C.Hunspell_free_list(c.handle, &carray, length)

	words := goStringSlice(carray, int(length))

	return words
}

func (c *Checker) Spell(word string) bool {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	c.m.Lock()
	res := C.Hunspell_spell(c.handle, cWord)
	c.m.Unlock()

	return int(res) != 0
}

func goStringSlice(c **C.char, l int) []string {
	s := make([]string, l)
	cArray := unsafe.Slice(c, l)

	for i, v := range cArray {
		s[i] = C.GoString(v)
	}

	return s
}
