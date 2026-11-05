package clitest_test

import "os/exec"

func runBare(bin string) error { return exec.Command(bin).Run() }
