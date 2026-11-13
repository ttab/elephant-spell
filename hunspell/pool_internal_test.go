package hunspell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAff = "../dictionaries/sv_SE.aff"
	testDic = "../dictionaries/sv_SE.dic"
)

func newTestChecker(t *testing.T, handles int) *Checker {
	t.Helper()

	c, err := NewChecker(testAff, testDic, WithHandles(handles))
	if err != nil {
		t.Fatalf("create checker: %v", err)
	}

	return c
}

// waitCtx bounds a wait that should succeed, so a deadlock fails the test
// rather than hanging it.
func waitCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func TestMissingDictionaryFile(t *testing.T) {
	_, err := NewChecker(testAff, "../dictionaries/xx_XX.dic")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing .dic: got error %v, want fs.ErrNotExist", err)
	}

	_, err = NewChecker("../dictionaries/xx_XX.aff", testDic)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing .aff: got error %v, want fs.ErrNotExist", err)
	}

	_, err = NewCheckerFS(os.DirFS("../dictionaries"),
		"sv_SE.aff", "xx_XX.dic")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing .dic in FS: got error %v, want fs.ErrNotExist", err)
	}

	c, err := NewCheckerFS(os.DirFS("../dictionaries"),
		"sv_SE.aff", "sv_SE.dic")
	if err != nil {
		t.Fatalf("create checker from FS: %v", err)
	}

	ok, err := c.Spell(t.Context(), "skola")
	if err != nil || !ok {
		t.Errorf("spell skola from FS checker = %v, %v; want true", ok, err)
	}
}

// TestCallsRunWhileAHandleIsBusy checks that a call does not wait for a
// busy handle when the pool can load another, and that the pool stops at its
// maximum.
func TestCallsRunWhileAHandleIsBusy(t *testing.T) {
	c := newTestChecker(t, 2)

	held, err := c.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ok, err := c.Spell(waitCtx(t), "skola")
	if err != nil {
		t.Fatalf("spell while a handle is held: %v", err)
	}

	if !ok {
		t.Error("skola should be accepted")
	}

	sugg, err := c.Suggest(waitCtx(t), "paralell")
	if err != nil {
		t.Fatalf("suggest while a handle is held: %v", err)
	}

	if len(sugg) == 0 || sugg[0] != "parallell" {
		t.Errorf("suggestions for paralell = %q", sugg)
	}

	c.release(held)

	st := c.Stats()
	if st.Handles != 2 || st.MaxHandles != 2 {
		t.Errorf("handles = %d of %d, want 2 of 2",
			st.Handles, st.MaxHandles)
	}

	if st.Waits < 1 {
		t.Errorf("waits = %d, want at least 1", st.Waits)
	}

	if st.InUse != 0 {
		t.Errorf("in use = %d after release, want 0", st.InUse)
	}
}

// TestWaitRespectsContext checks that a call waiting for a handle gives up
// when its context ends.
func TestWaitRespectsContext(t *testing.T) {
	c := newTestChecker(t, 1)

	held, err := c.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	defer c.release(held)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err = c.Suggest(ctx, "paralell")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("suggest with every handle busy: got %v, want %v",
			err, context.DeadlineExceeded)
	}

	st := c.Stats()
	if st.Handles != 1 {
		t.Errorf("handles = %d, want 1", st.Handles)
	}

	if st.Waits != 1 || st.CanceledWaits != 1 || st.WaitDuration <= 0 {
		t.Errorf("waits = %d, canceled = %d, wait time = %v; want 1, 1, > 0",
			st.Waits, st.CanceledWaits, st.WaitDuration)
	}

	if st.InUse != 1 {
		t.Errorf("in use = %d, want 1", st.InUse)
	}
}

// holdAll takes n handles out of the pool, growing it to n, and returns
// them.
func holdAll(t *testing.T, c *Checker, n int) []*handleRef {
	t.Helper()

	var held []*handleRef

	for range n {
		h, err := c.acquire(waitCtx(t))
		if err != nil {
			t.Fatalf("acquire handle %d: %v", len(held)+1, err)
		}

		held = append(held, &handleRef{c: c, h: h})
	}

	return held
}

// TestUpdatesReachEveryHandle checks that added and removed words reach
// every handle, including handles loaded after the update.
func TestUpdatesReachEveryHandle(t *testing.T) {
	c := newTestChecker(t, 3)

	const (
		gone = "qzxwvutsa"
		kept = "al-Fatiha"
	)

	// Updates made while the pool has one handle.
	if !c.Add(gone) || !c.Remove(gone) || !c.Add(kept) {
		t.Fatal("updates were refused")
	}

	held := holdAll(t, c, 3)

	for i, h := range held {
		if !h.spell(kept) {
			t.Errorf("handle %d does not accept %q", i+1, kept)
		}

		if h.spell(gone) {
			t.Errorf("handle %d accepts removed %q", i+1, gone)
		}
	}

	for _, h := range held {
		h.release()
	}

	// An update made with three handles reaches all three.
	if !c.Remove(kept) {
		t.Fatal("remove was refused")
	}

	held = holdAll(t, c, 3)

	for i, h := range held {
		if h.spell(kept) {
			t.Errorf("handle %d accepts removed %q", i+1, kept)
		}
	}

	for _, h := range held {
		h.release()
	}
}

// updateLogLen returns the number of updates kept for replay on handles
// loaded later.
func updateLogLen(c *Checker) int {
	c.write.Lock()
	defer c.write.Unlock()

	return len(c.log)
}

// TestFullPoolKeepsNoUpdateLog checks that updates are only kept for replay
// while the pool can still grow, so a full pool does not accumulate every
// update for the life of the process.
func TestFullPoolKeepsNoUpdateLog(t *testing.T) {
	single := newTestChecker(t, 1)

	if !single.Add("qzxwvutsa") {
		t.Fatal("add was refused")
	}

	if n := updateLogLen(single); n != 0 {
		t.Errorf("single-handle pool keeps %d updates, want 0", n)
	}

	c := newTestChecker(t, 2)

	const (
		early = "qzxwvutsa"
		late  = "al-Fatiha"
	)

	if !c.Add(early) {
		t.Fatal("add was refused")
	}

	if n := updateLogLen(c); n != 1 {
		t.Fatalf("growable pool keeps %d updates, want 1", n)
	}

	held := holdAll(t, c, 2)

	for _, h := range held {
		h.release()
	}

	if n := updateLogLen(c); n != 0 {
		t.Errorf("full pool keeps %d updates, want 0", n)
	}

	if !c.Add(late) {
		t.Fatal("add was refused")
	}

	if n := updateLogLen(c); n != 0 {
		t.Errorf("full pool keeps %d updates after an add, want 0", n)
	}

	held = holdAll(t, c, 2)

	for i, h := range held {
		for _, w := range []string{early, late} {
			if !h.spell(w) {
				t.Errorf("handle %d does not accept %q", i+1, w)
			}
		}
	}

	for _, h := range held {
		h.release()
	}
}

// TestConcurrentChecksAndUpdates runs checks against updates and checks that
// a call never sees a word that has been added on some handles but not on
// the one it got. Run it with -race.
func TestConcurrentChecksAndUpdates(t *testing.T) {
	const (
		readers = 8
		rounds  = 40
	)

	c := newTestChecker(t, 4)

	// added is the number of words the writer has finished adding.
	var added atomic.Int64

	ctx, cancel := context.WithCancel(waitCtx(t))
	defer cancel()

	var (
		wg       sync.WaitGroup
		failures atomic.Int64
	)

	for r := range readers {
		wg.Go(func() {
			for ctx.Err() == nil {
				n := added.Load()
				if n == 0 {
					continue
				}

				word := fmt.Sprintf("qzkept%d", n-1)

				ok, err := c.Spell(ctx, word)
				if errors.Is(err, context.Canceled) {
					return
				}

				if err != nil || !ok {
					failures.Add(1)
					t.Errorf("reader %d: spell %q = %v, %v; want true",
						r, word, ok, err)

					return
				}

				if r%2 == 0 {
					_, err = c.Suggest(ctx, "paralell")
				} else {
					_, err = c.Stem(ctx, "skolorna")
				}

				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("reader %d: %v", r, err)

					return
				}
			}
		})
	}

	// Keep updating until the pool has grown to its maximum, so that
	// handles are loaded, and replay the log, while updates run.
	n := 0

	for ; n < rounds || c.Stats().Handles < 4; n++ {
		if failures.Load() > 0 || ctx.Err() != nil {
			break
		}

		i := n

		temp := fmt.Sprintf("qztemp%d", i)

		if !c.Add(fmt.Sprintf("qzkept%d", i)) || !c.Add(temp) {
			t.Fatalf("round %d: add refused", i)
		}

		added.Store(int64(i + 1))

		if !c.Remove(temp) {
			t.Fatalf("round %d: remove refused", i)
		}
	}

	cancel()
	wg.Wait()

	if c.Stats().Handles != 4 {
		t.Errorf("the pool grew to %d handles under %d readers, want 4",
			c.Stats().Handles, readers)
	}

	for _, h := range holdAll(t, c, c.Stats().Handles) {
		for i := range n {
			if !h.spell(fmt.Sprintf("qzkept%d", i)) {
				t.Errorf("a handle does not accept qzkept%d", i)
			}

			if h.spell(fmt.Sprintf("qztemp%d", i)) {
				t.Errorf("a handle accepts removed qztemp%d", i)
			}
		}

		h.release()
	}
}

// handleRef is a handle a test holds out of the pool.
type handleRef struct {
	c *Checker
	h handle
}

func (r *handleRef) spell(word string) bool {
	return spellOnHandle(r.h, word)
}

func (r *handleRef) release() {
	r.c.release(r.h)
}
