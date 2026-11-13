// Package hunspell wraps libhunspell behind a pool of handles per dictionary.
//
// A hunspell handle is not safe for concurrent use, and a suggestion search on
// one takes tens of milliseconds, so a Checker holds several handles loaded
// from the same dictionary and lends one to each call. It starts with one
// handle and loads another, up to its maximum, whenever a call finds every
// handle busy. Custom words are added to and removed from every handle, with
// each handle taken out of the pool while it is changed.
package hunspell

// #cgo linux LDFLAGS: -lhunspell
//
// #include <stdlib.h>
// #include <stdio.h>
// #include <hunspell/hunspell.h>
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// DefaultHandles is the default maximum number of hunspell handles a Checker
// holds for its dictionary, and so the number of calls that can run on it at
// the same time.
const DefaultHandles = 4

// Option configures a Checker.
type Option func(c *Checker)

// WithHandles sets the maximum number of handles the Checker loads. Values
// below one are treated as one.
func WithHandles(n int) Option {
	return func(c *Checker) {
		c.max = max(n, 1)
	}
}

// Checker spellchecks against one dictionary.
type Checker struct {
	load func() (handle, error)
	max  int

	// idle holds the handles that no call is using.
	idle chan handle

	// write serialises updates and pool growth, and guards log.
	write sync.Mutex
	// log is every update in order, replayed on a handle loaded after
	// it. Kept whole rather than compacted per word: hunspell's add and
	// remove change a word's affix flags, so their effect depends on the
	// whole history of the word. Empty when the pool cannot grow.
	log []wordUpdate

	set *handleSet

	growing        atomic.Bool
	waiting        atomic.Int64
	size           atomic.Int64
	inUse          atomic.Int64
	waits          atomic.Int64
	waitNanos      atomic.Int64
	canceledWaits  atomic.Int64
	createFailures atomic.Int64
}

// handle is a loaded hunspell dictionary. It is not safe for concurrent use.
type handle = *C.Hunhandle

type wordUpdate struct {
	word string
	add  bool
}

// handleSet owns every handle a Checker has loaded, so that the cleanup can
// destroy them without keeping the Checker reachable.
type handleSet struct {
	m       sync.Mutex
	handles []handle
}

func (s *handleSet) add(h handle) {
	s.m.Lock()
	s.handles = append(s.handles, h)
	s.m.Unlock()
}

func destroyHandles(s *handleSet) {
	s.m.Lock()
	defer s.m.Unlock()

	for _, h := range s.handles {
		C.Hunspell_destroy(h)
	}

	s.handles = nil
}

// NewChecker creates a checker for the dictionary in the files affixPath and
// dictPath. The files are read again whenever the pool grows, so they have to
// stay in place for as long as the checker is used.
func NewChecker(
	affixPath string, dictPath string, opts ...Option,
) (*Checker, error) {
	load := func() (handle, error) {
		return createHandle(affixPath, dictPath)
	}

	return newChecker(load, opts)
}

// NewCheckerFS creates a checker for the dictionary in the files affixName and
// dictName in fsys. Hunspell only reads from the file system, so every handle
// is loaded from a copy in a temporary directory that is removed once the
// handle has been created.
func NewCheckerFS(
	fsys fs.FS, affixName string, dictName string, opts ...Option,
) (*Checker, error) {
	load := func() (_ handle, outErr error) {
		dir, err := os.MkdirTemp("", "hunspell-*")
		if err != nil {
			return nil, fmt.Errorf("create dictionary directory: %w", err)
		}

		defer func() {
			err := os.RemoveAll(dir)
			if err != nil {
				outErr = errors.Join(outErr, fmt.Errorf(
					"remove dictionary directory: %w", err))
			}
		}()

		affixPath := filepath.Join(dir, "dict.aff")
		dictPath := filepath.Join(dir, "dict.dic")

		for name, dst := range map[string]string{
			affixName: affixPath,
			dictName:  dictPath,
		} {
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", name, err)
			}

			err = os.WriteFile(dst, data, 0o600)
			if err != nil {
				return nil, fmt.Errorf("copy %q: %w", name, err)
			}
		}

		return createHandle(affixPath, dictPath)
	}

	return newChecker(load, opts)
}

func newChecker(
	load func() (handle, error), opts []Option,
) (*Checker, error) {
	c := Checker{
		load: load,
		max:  DefaultHandles,
		set:  &handleSet{},
	}

	for _, o := range opts {
		o(&c)
	}

	h, err := load()
	if err != nil {
		return nil, err
	}

	c.idle = make(chan handle, c.max)
	c.idle <- h

	c.set.add(h)
	c.size.Store(1)

	runtime.AddCleanup(&c, destroyHandles, c.set)

	return &c, nil
}

// createHandle loads a dictionary. Hunspell_create reports nothing when it
// cannot read a file and returns a handle that accepts no words, so the files
// are checked first.
func createHandle(affixPath string, dictPath string) (handle, error) {
	for _, p := range []string{affixPath, dictPath} {
		_, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("open dictionary file: %w", err)
		}
	}

	cAffPath := C.CString(affixPath)
	defer C.free(unsafe.Pointer(cAffPath))

	cDictPath := C.CString(dictPath)
	defer C.free(unsafe.Pointer(cDictPath))

	h := C.Hunspell_create(cAffPath, cDictPath)
	if h == nil {
		return nil, errors.New("hunspell could not load the dictionary")
	}

	return h, nil
}

// acquire takes a handle out of the pool, waiting for one when all are busy
// until the context ends. A caller that finds every handle busy also starts
// loading another one, if the pool has room for it.
func (c *Checker) acquire(ctx context.Context) (handle, error) {
	select {
	case h := <-c.idle:
		c.inUse.Add(1)

		return h, nil
	default:
	}

	c.waits.Add(1)
	c.waiting.Add(1)
	defer c.waiting.Add(-1)

	c.grow()

	start := time.Now()

	select {
	case h := <-c.idle:
		c.waitNanos.Add(int64(time.Since(start)))
		c.inUse.Add(1)

		return h, nil
	case <-ctx.Done():
		c.waitNanos.Add(int64(time.Since(start)))
		c.canceledWaits.Add(1)

		return nil, fmt.Errorf("wait for a hunspell handle: %w",
			context.Cause(ctx))
	}
}

func (c *Checker) release(h handle) {
	c.inUse.Add(-1)
	c.idle <- h
}

// grow loads one more handle in the background, unless the pool is full or a
// handle is already being loaded.
func (c *Checker) grow() {
	if c.size.Load() >= int64(c.max) {
		return
	}

	if !c.growing.CompareAndSwap(false, true) {
		return
	}

	go func() {
		// Loading takes up to a second for the largest dictionaries,
		// so it happens outside the write lock.
		h, err := c.load()
		if err != nil {
			c.createFailures.Add(1)
			c.growing.Store(false)

			return
		}

		c.write.Lock()

		for _, u := range c.log {
			applyUpdate(h, u)
		}

		c.set.add(h)
		c.size.Add(1)

		// Done growing before the handle is handed out, so that a
		// call that is still left waiting after it can grow the pool
		// again.
		c.growing.Store(false)

		// The channel has room for every handle, so this never
		// blocks.
		c.idle <- h

		c.write.Unlock()

		// Keep growing while calls are waiting. The call that gets
		// this handle may not have stopped counting itself yet, which
		// can cost one handle more than needed under a burst.
		if c.waiting.Load() > 0 {
			c.grow()
		}
	}()
}

// Suggest returns hunspell's suggestions for a word. It returns an error only
// when the context ends while it waits for a handle.
func (c *Checker) Suggest(ctx context.Context, word string) ([]string, error) {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	h, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}

	defer c.release(h)

	var cArray **C.char

	length := C.Hunspell_suggest(h, &cArray, cWord)

	words := goStringSlice(cArray, int(length))

	// The list belongs to the handle, so it is freed before the handle
	// goes back to the pool.
	C.Hunspell_free_list(h, &cArray, length)

	return words, nil
}

// Stem returns the stems of a word. It returns an error only when the context
// ends while it waits for a handle.
func (c *Checker) Stem(ctx context.Context, word string) ([]string, error) {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	h, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}

	defer c.release(h)

	var cArray **C.char

	length := C.Hunspell_stem(h, &cArray, cWord)

	words := goStringSlice(cArray, int(length))

	C.Hunspell_free_list(h, &cArray, length)

	return words, nil
}

// Spell reports whether hunspell accepts a word. It returns an error only
// when the context ends while it waits for a handle.
func (c *Checker) Spell(ctx context.Context, word string) (bool, error) {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	h, err := c.acquire(ctx)
	if err != nil {
		return false, err
	}

	defer c.release(h)

	return spellOn(h, cWord), nil
}

func spellOn(h handle, cWord *C.char) bool {
	return int(C.Hunspell_spell(h, cWord)) != 0
}

// Add adds a word to every handle, and to every handle loaded later. It waits
// for each handle to be returned to the pool, without a timeout, and reports
// whether hunspell accepted the word on all of them.
func (c *Checker) Add(word string) bool {
	return c.update(wordUpdate{word: word, add: true})
}

// Remove removes a word from every handle, and from every handle loaded
// later. It waits as Add does.
func (c *Checker) Remove(word string) bool {
	return c.update(wordUpdate{word: word})
}

// update applies a word change to every handle. It takes all the handles out
// of the pool before it changes any of them, so a call sees either none of
// the handles changed or all of them.
func (c *Checker) update(u wordUpdate) bool {
	c.write.Lock()
	defer c.write.Unlock()

	if c.max > 1 {
		c.log = append(c.log, u)
	}

	// Growth only adds handles under the write lock, so the size holds
	// still while the handles are collected.
	n := int(c.size.Load())
	held := make([]handle, 0, n)

	for range n {
		held = append(held, <-c.idle)
	}

	ok := true

	for _, h := range held {
		ok = applyUpdate(h, u) && ok
	}

	for _, h := range held {
		c.idle <- h
	}

	return ok
}

func applyUpdate(h handle, u wordUpdate) bool {
	cWord := C.CString(u.word)
	defer C.free(unsafe.Pointer(cWord))

	var r C.int

	if u.add {
		r = C.Hunspell_add(h, cWord)
	} else {
		r = C.Hunspell_remove(h, cWord)
	}

	return int(r) == 0
}

// Stats describes the handle pool.
type Stats struct {
	// Handles is the number of handles loaded.
	Handles int
	// MaxHandles is the number of handles the pool may grow to.
	MaxHandles int
	// InUse is the number of handles lent to calls.
	InUse int
	// Waits is the number of calls that found every handle busy.
	Waits int64
	// WaitDuration is the total time calls have waited for a handle.
	WaitDuration time.Duration
	// CanceledWaits is the number of calls whose context ended before
	// they got a handle.
	CanceledWaits int64
	// CreateFailures is the number of times growing the pool failed to
	// load a handle.
	CreateFailures int64
}

// Stats returns the current state of the handle pool.
func (c *Checker) Stats() Stats {
	return Stats{
		Handles:        int(c.size.Load()),
		MaxHandles:     c.max,
		InUse:          int(c.inUse.Load()),
		Waits:          c.waits.Load(),
		WaitDuration:   time.Duration(c.waitNanos.Load()),
		CanceledWaits:  c.canceledWaits.Load(),
		CreateFailures: c.createFailures.Load(),
	}
}

func goStringSlice(c **C.char, l int) []string {
	s := make([]string, l)
	cArray := unsafe.Slice(c, l)

	for i, v := range cArray {
		s[i] = C.GoString(v)
	}

	return s
}

// spellOnHandle is spellOn for tests, which cannot use cgo.
func spellOnHandle(h handle, word string) bool {
	cWord := C.CString(word)
	defer C.free(unsafe.Pointer(cWord))

	return spellOn(h, cWord)
}
