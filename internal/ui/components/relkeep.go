package components

import "time"

// RelKeeper holds the last window that loaded, so a stream whose read fails
// keeps the bar's counts instead of showing zeros (releases view RFC,
// section 4: "as of 21:04"). One per stream.
type RelKeeper struct {
	good RelWindow
	at   time.Time
	ok   bool
}

// Hold answers w when it loaded; when it did not, the last good window with
// the cannot-load line, its counts marked with their time and w's Retry. A
// first read that fails has nothing to keep: w as it is.
func (k *RelKeeper) Hold(w *RelWindow, now time.Time) *RelWindow {
	if w == nil {
		return nil
	}
	if !w.Err {
		k.good, k.at, k.ok = *w, now, true
		return w
	}
	if !k.ok {
		return w
	}
	kept := k.good
	kept.Err, kept.AsOf, kept.Retry, kept.Open = true, k.at.Format("15:04"), w.Retry, w.Open
	return &kept
}
