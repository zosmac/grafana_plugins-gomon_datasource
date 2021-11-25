// Copyright © 2021 The Gomon Project.

//go:build !windows
// +build !windows

package main

/*
#include <libproc.h>
*/
import "C"

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

var (
	// regex for parsing lsof output lines from lsof command.
	regex = regexp.MustCompile(
		`^(?:(?P<header>COMMAND.*)|====(?P<trailer>\d\d:\d\d:\d\d)====.*|` +
			`(?P<command>[^ ]+)[ ]+` +
			`(?P<pid>[^ ]+)[ ]+` +
			`(?:[^ ]+)[ ]+` + // USER
			`(?P<fd>(?:\d+|fp\.))` +
			`(?P<mode>[ rwu])[ ]+` +
			`(?P<type>(?:[^ ]+|))[ ]+` +
			`(?P<device>(?:0x[0-9a-f]+|\d+,\d+|kpipe|upipe|))[ ]+` +
			`(?:[^ ]+|)[ ]+` + // SIZE/OFF
			`(?P<node>(?:\d+|TCP|UDP|))[ ]+` +
			`(?P<name>.*))$`,
	)

	// rgxgroups maps names of capture groups to indices.
	rgxgroups = func() map[captureGroup]int {
		g := map[captureGroup]int{}
		for _, name := range regex.SubexpNames() {
			g[captureGroup(name)] = regex.SubexpIndex(name)
		}
		return g
	}()
)

const (
	// lsof line regular expressions named capture groups.
	groupHeader  captureGroup = "header"
	groupTrailer captureGroup = "trailer"
	groupCommand captureGroup = "command"
	groupPid     captureGroup = "pid"
	groupFd      captureGroup = "fd"
	groupMode    captureGroup = "mode"
	groupType    captureGroup = "type"
	groupDevice  captureGroup = "device"
	groupNode    captureGroup = "node"
	groupName    captureGroup = "name"
)

type (
	// captureGroup is the name of a reqular expression capture group.
	captureGroup string
)

// lsofCommand starts the lsof command to capture process connections
func lsofCommand(ready chan<- struct{}) {
	cmd := hostCommand() // perform OS specific customizations for command

	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			buf = buf[:n]
			log.DefaultLogger.Error("Command panicked",
				"command", cmd.String(),
				"pid", strconv.Itoa(cmd.Process.Pid), // to format as int rather than float
				"panic", r,
				"stacktrace", string(buf),
			)
		}
	}()

	log.DefaultLogger.Info("Fork command to capture open process descriptors",
		"command", cmd.String(),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.DefaultLogger.Error("Pipe to stdout failed",
			"command", cmd.String(),
			"err", err,
		)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.DefaultLogger.Error("Pipe to stderr failed",
			"command", cmd.String(),
			"err", err,
		)
		return
	}

	if err := cmd.Start(); err != nil {
		log.DefaultLogger.Error("Command failed",
			"command", cmd.String(),
			"pid", strconv.Itoa(cmd.Process.Pid), // to format as int rather than float
			"err", err,
		)
		return
	}

	ready <- struct{}{}

	epm := map[Pid]Connections{}

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		match := regex.FindStringSubmatch(sc.Text())
		if len(match) == 0 || match[0] == "" {
			continue
		}
		if header := match[rgxgroups[groupHeader]]; header != "" {
			continue
		}
		if trailer := match[rgxgroups[groupTrailer]]; trailer != "" {
			epLock.Lock()
			epMap = epm
			epm = map[Pid]Connections{}
			epLock.Unlock()
			continue
		}

		command := match[rgxgroups[groupCommand]]
		pid, _ := strconv.Atoi(match[rgxgroups[groupPid]])
		fd, _ := strconv.Atoi(match[rgxgroups[groupFd]])
		mode := match[rgxgroups[groupMode]]
		fdType := match[rgxgroups[groupType]]
		device := match[rgxgroups[groupDevice]]
		node := match[rgxgroups[groupNode]]
		name := match[rgxgroups[groupName]]

		var self, peer string

		switch fdType {
		case "BLK", "DIR", "REG", "LINK",
			"CHAN", "FSEVENT", "KQUEUE", "NEXUS", "NPOLICY", "PSXSHM":
		case "CHR":
			if name == os.DevNull {
				fdType = "NUL"
			}
		case "FIFO":
			if mode == "w" {
				peer = name
			} else {
				self = name
			}
		case "PIPE", "unix":
			peer = name
			if len(peer) > 2 && peer[:2] == "->" {
				peer = peer[2:] // strip "->"
			}
			name = device
			self = device
		case "IPv4", "IPv6":
			var state string
			fdType = node
			split := strings.Split(name, " ")
			if len(split) > 1 {
				state = split[0]
			}
			split = strings.Split(split[0], "->")
			self = split[0]
			if len(split) == 2 {
				peer = strings.Split(split[1], " ")[0]
			} else {
				self += " " + state
			}
			name = device
		case "systm":
			self = device
			peer = "kernel"
		case "key":
			name = device
			self = device
		case "PSXSEM":
			self = device
			peer = device
		}

		ep := Connection{
			Descriptor: fd,
			Type:       fdType,
			Name:       name,
			Self:       self,
			Peer:       peer,
		}

		log.DefaultLogger.Debug("Endpoint",
			"name", name,
			"command", command,
			"pid", strconv.Itoa(pid), // to format as int rather than float
			"fd", strconv.Itoa(fd), // to format as int rather than float
			"type", fdType,
			"self", self,
			"peer", peer,
		)

		epm[Pid(pid)] = append(epm[Pid(pid)], ep)
	}

	log.DefaultLogger.Error("Scanning output failed",
		"command", cmd.String(),
		"pid", strconv.Itoa(cmd.Process.Pid), // to format as int rather than float
		"err", sc.Err(),
	)

	if buf, err := io.ReadAll(stderr); err != nil || len(buf) > 0 {
		log.DefaultLogger.Error("Command error log",
			"command", cmd.String(),
			"pid", strconv.Itoa(cmd.Process.Pid), // to format as int rather than float
			"err", err,
			"stderr", string(buf),
		)
	}

	err = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	log.DefaultLogger.Error("Command failed",
		"command", cmd.String(),
		"pid", strconv.Itoa(cmd.Process.Pid), // to format as int rather than float
		"code", strconv.Itoa(code), // to format as int rather than float
		"err", err,
	)

	os.Exit(code)
}
