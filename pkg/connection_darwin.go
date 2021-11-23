// Copyright © 2021 The Gomon Project.

package main

/*
#include <libproc.h>
*/
import "C"

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func hostCommand() *exec.Cmd {
	cmdline := strings.Fields(fmt.Sprintf("lsof -S2 -X -r%dm====%%T====", 10))
	cmd := exec.Command(cmdline[0], cmdline[1:]...)

	// ensure that no open descriptors propagate to child
	if n := C.proc_pidinfo(
		C.int(os.Getpid()),
		C.PROC_PIDLISTFDS,
		0,
		nil,
		0,
	); n >= 3*C.PROC_PIDLISTFD_SIZE {
		cmd.ExtraFiles = make([]*os.File, (n/C.PROC_PIDLISTFD_SIZE)-3) // close gomon files in child
	}

	return cmd
}
