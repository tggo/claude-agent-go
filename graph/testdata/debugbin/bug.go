//go:build !fixed

package main

import "errors"

const build = "buggy"

// n3 has the bug the tests "fix": it always fails.
func n3(*state) error { return errors.New("n3: index out of range [3] with length 3") }
