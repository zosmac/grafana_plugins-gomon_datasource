// Copyright © 2021 The Gomon Project.

//go:build !windows
// +build !windows

package process

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
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
			`(?:(?P<fd>\d+)|fp\.|mem|cwd|rtd)` +
			`(?P<mode> |[rwu-][rwuNRWU]?)[ ]+` +
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

func init() {
	err := lsofCommand()
	setuid() // after lsof command starts, set to the grafana user
	if err != nil {
		log.DefaultLogger.Error("command to capture open process descriptors failed",
			"error", err,
		)
	}
}

// lsofCommand starts the lsof command to capture process connections.
func lsofCommand() error {
	cmd := hostCommand() // perform OS specific customizations for command
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe failed %w", err)
	}
	cmd.Stderr = nil // sets to /dev/null
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start failed %w", err)
	}

	log.DefaultLogger.Info("start command to capture open process descriptors",
		"command", cmd.String(),
		"pid", strconv.Itoa(cmd.Process.Pid), // to format as int rather than float
	)

	go parseOutput(stdout)

	return nil
}

// parseOutput reads the stdout of the command.
func parseOutput(stdout io.ReadCloser) {
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
		mode := match[rgxgroups[groupMode]][0]
		fdType := match[rgxgroups[groupType]]
		device := match[rgxgroups[groupDevice]]
		node := match[rgxgroups[groupNode]]
		name := match[rgxgroups[groupName]]

		var self, peer string

		switch fdType {
		case "LINK", "CHAN", "FSEVENT", "KQUEUE", "NEXUS", "NPOLICY",
			"ndrv", "unknown":
		case "BLK", "DIR", "REG", "PSXSHM":
			self = name
			peer = name
		case "CHR":
			if name == os.DevNull {
				fdType = "NUL"
			}
		case "FIFO":
			if mode == 'w' {
				peer = name
			} else {
				self = name
			}
		case "PIPE", "unix":
			self = device
			peer = name
			if len(peer) > 2 && peer[:2] == "->" {
				peer = peer[2:] // strip "->"
			}
			name = self + "->" + peer
		case "IPv4", "IPv6":
			fdType = node
			split := strings.Split(name, " ")
			split = strings.Split(split[0], "->")
			self = split[0]
			if len(split) > 1 {
				peer = split[1]
			}
		case "systm":
			self = device
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

	panic(fmt.Errorf("stdout closed %v", sc.Err()))
}
