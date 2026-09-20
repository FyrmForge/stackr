// Package guard holds the repo-wide architecture tests: the walks that fail
// when a rule the codebase depends on stops being true. It has no production
// code and is imported by nothing.
//
// The rules live here rather than next to the code they police because they
// are about the whole tree. handlers/web/storefree_test.go predates this
// package and stays where it is — it polices one tree and reads better there.
package guard
