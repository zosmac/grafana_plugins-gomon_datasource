// Copyright © 2021-2023 The Gomon Project.

package plugin

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/zosmac/gocore"
	"github.com/zosmac/gomon/process"
)

type (
	// Pid alias for Pid in process package.
	Pid = process.Pid
)

var (
	// host/proc specify the arc for the circle drawn around a node.
	// Each arc has a specific color set in its field metadata to create a circle that identifies the node type.
	hostArc = []any{1.0, 0.0, 0.0, 0.0, 0.0} // red
	procArc = []any{0.0, 1.0, 0.0, 0.0, 0.0} // blue
	dataArc = []any{0.0, 0.0, 1.0, 0.0, 0.0} // yellow
	sockArc = []any{0.0, 0.0, 0.0, 1.0, 0.0} // magenta
	kernArc = []any{0.0, 0.0, 0.0, 0.0, 1.0} // cyan
	red     = map[string]any{"mode": "fixed", "fixedColor": "red"}
	blue    = map[string]any{"mode": "fixed", "fixedColor": "blue"}
	yellow  = map[string]any{"mode": "fixed", "fixedColor": "yellow"}
	magenta = map[string]any{"mode": "fixed", "fixedColor": "magenta"}
	cyan    = map[string]any{"mode": "fixed", "fixedColor": "cyan"}
)

// Nodegraph produces the process connections node graph.
func Nodegraph(link string, pid Pid) (resp backend.DataResponse) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			buf = buf[:n]
			gocore.Error("nodegraph panic", nil, map[string]string{
				"panic":      fmt.Sprint(r),
				"stacktrace": string(buf),
			}).Err()
			if e, ok := r.(error); ok {
				resp.Error = e
			} else {
				resp.Error = fmt.Errorf("nodegraph panic: %v", r)
			}
		}
	}()

	gocore.Error("nodegraph", nil, map[string]string{
		"pid": pid.String(),
	}).Info()

	tb := process.BuildTable()
	tr := process.BuildTree(tb)
	process.Connections(tb)

	if pid != 0 && tb[pid] == nil {
		pid = 0 // reset to default
	}

	pt := process.Table{}
	if pid > 0 { // build this process' "extended family"
		pt = family(tb, tr, pid)
	} else { // only consider non-daemon and remote host connected processes
		for pid, p := range tb {
			if p.Ppid > 1 {
				for pid, p := range family(tb, tr, pid) {
					pt[pid] = p
				}
			}
			for _, conn := range p.Connections {
				if conn.Peer.Pid < 0 {
					pt[conn.Self.Pid] = tb[conn.Self.Pid]
				}
			}
		}
	}

	nm := map[Pid][]any{}
	em := map[string][]any{}
	timestamp := time.Now()

	for _, p := range pt {
		for _, conn := range p.Connections {
			if conn.Self.Pid == 0 || conn.Peer.Pid == 0 || // ignore kernel process
				conn.Self.Pid == 1 || conn.Peer.Pid == 1 || // ignore launchd processes
				conn.Self.Pid == conn.Peer.Pid || // ignore inter-process connections
				pid == 0 && conn.Peer.Pid >= math.MaxInt32 { // ignore data connections for the "all process" query
				continue
			}

			nm[conn.Self.Pid] = append([]any{
				timestamp,
				int64(conn.Self.Pid),
				tb[conn.Self.Pid].Id.Name,
				conn.Self.Pid.String(),
				longname(tb, conn.Self.Pid),
				longname(tb, tb[conn.Self.Pid].Ppid),
			}, procArc...)

			if conn.Peer.Pid < 0 { // peer is remote host or listener
				host, port, _ := net.SplitHostPort(conn.Peer.Name)

				arc := hostArc
				// name for listen port is device inode: on linux decimal and on darwin hexadecimal
				if _, err := strconv.Atoi(conn.Self.Name); err == nil || conn.Self.Name[0:2] == "0x" { // listen socket
					arc = sockArc
				}

				nm[conn.Peer.Pid] = append([]any{
					timestamp,
					int64(conn.Peer.Pid),
					conn.Type + ":" + port,
					gocore.Hostname(host),
					host,
					gocore.Hostname(host),
				}, arc...)

				// flip the source and target to get Host shown to left in node graph
				id := fmt.Sprintf("%d -> %d", conn.Peer.Pid, conn.Self.Pid)
				em[id] = []any{
					timestamp,
					id,
					int64(conn.Peer.Pid),
					int64(conn.Self.Pid),
					host,
					shortname(tb, conn.Self.Pid),
					fmt.Sprintf(
						"%s:%s ‑> %s", // non-breaking space/hyphen
						conn.Type,
						conn.Peer.Name,
						conn.Self.Name,
					),
				}
			} else if conn.Peer.Pid >= math.MaxInt32 { // peer is data
				peer := conn.Type + ":" + conn.Peer.Name

				arc := dataArc
				if conn.Type != "REG" && conn.Type != "DIR" {
					arc = kernArc
				}

				nm[conn.Peer.Pid] = append([]any{
					timestamp,
					int64(conn.Peer.Pid),
					conn.Type,
					conn.Peer.Name,
					peer,
					shortname(tb, conn.Self.Pid),
				}, arc...)

				// show edge for data connections only once
				id := fmt.Sprintf("%d -> %d", conn.Self.Pid, conn.Peer.Pid)
				if _, ok := em[id]; !ok {
					em[id] = []any{
						timestamp,
						id,
						int64(conn.Self.Pid),
						int64(conn.Peer.Pid),
						shortname(tb, conn.Self.Pid),
						url.QueryEscape(peer),
						fmt.Sprintf(
							"%s:%s ‑> %s", // non-breaking space/hyphen
							conn.Type,
							conn.Self.Name,
							conn.Peer.Name,
						),
					}
				}
			} else { // peer is process
				peer := shortname(tb, conn.Peer.Pid)
				nm[conn.Peer.Pid] = append([]any{
					timestamp,
					int64(conn.Peer.Pid),
					tb[conn.Peer.Pid].Id.Name,
					conn.Peer.Pid.String(),
					longname(tb, conn.Peer.Pid),
					longname(tb, tb[conn.Peer.Pid].Ppid),
				}, procArc...)

				// show edge for inter-process connections only once
				id := fmt.Sprintf("%d -> %d", conn.Self.Pid, conn.Peer.Pid)
				di := fmt.Sprintf("%d -> %d", conn.Peer.Pid, conn.Self.Pid)

				_, ok := em[id]
				if ok {
					em[id][6] = (em[id][6]).(string) + fmt.Sprintf(
						"\n%s:%s ‑> %s", // non-breaking space/hyphen
						conn.Type,
						conn.Self.Name,
						conn.Peer.Name,
					)
				} else if _, ok = em[di]; ok {
					em[di][6] = (em[di][6]).(string) + fmt.Sprintf(
						"\n%s:%s ‑> %s", // non-breaking space/hyphen
						conn.Type,
						conn.Peer.Name,
						conn.Self.Name,
					)
				} else {
					em[id] = []any{
						timestamp,
						id,
						int64(conn.Self.Pid),
						int64(conn.Peer.Pid),
						shortname(tb, conn.Self.Pid),
						peer,
						fmt.Sprintf(
							"%s ‑> %s\n%s:%s ‑> %s", // non-breaking space/hyphen
							shortname(tb, conn.Self.Pid),
							shortname(tb, conn.Peer.Pid),
							conn.Type,
							conn.Self.Name,
							conn.Peer.Name,
						),
					}
				}
			}
		}
	}

	ns := make([][]any, len(nm))
	i := 0
	for _, n := range nm {
		ns[i] = n
		i++
	}

	sort.Slice(ns, func(i, j int) bool {
		return ns[i][1].(int64) < ns[j][1].(int64)
	})

	es := make([][]any, len(em))
	i = 0
	for _, e := range em {
		es[i] = e
		i++
	}

	sort.Slice(es, func(i, j int) bool {
		return es[i][2].(int64) < es[j][2].(int64) ||
			es[i][2].(int64) == es[j][2].(int64) && es[i][3].(int64) < es[j][3].(int64)
	})

	resp.Frames = nodeFrames(link, ns, es)

	return
}

// family identifies all of the ancestor and children processes of a process.
func family(tb process.Table, tr process.Tree, pid Pid) process.Table {
	pt := process.Table{}
	for pid := pid; pid > 0; pid = tb[pid].Ppid { // ancestors
		pt[pid] = tb[pid]
	}

	tr = tr.FindTree(pid)
	o := func(node Pid, pt process.Table) int {
		return order(node, tr, pt)
	}

	pids := tr.Flatten(tb, o)
	for _, pid := range pids {
		pt[pid] = tb[pid]
	}

	return pt
}

// order returns the process' depth in the tree.
func order(node Pid, tr process.Tree, _ process.Table) int {
	var depth int
	for _, tr := range tr {
		dt := depthTree(tr) + 1
		if depth < dt {
			depth = dt
		}
	}
	return depth
}

// order returns the process' depth in the tree for sorting.
func depthTree(tr process.Tree) int {
	depth := 0
	for _, tr := range tr {
		dt := depthTree(tr) + 1
		if depth < dt {
			depth = dt
		}
	}
	return depth
}

// longname formats the full Executable name and pid.
func longname(tb process.Table, pid Pid) string {
	if p, ok := tb[pid]; ok {
		name := p.Executable
		if name == "" {
			name = p.Id.Name
		}
		return fmt.Sprintf("%s[%d]", name, pid)
	}
	return ""
}

// shortname formats process name and pid.
func shortname(tb process.Table, pid Pid) string {
	if p, ok := tb[pid]; ok {
		return fmt.Sprintf("%s[%d]", p.Id.Name, pid)
	}
	return ""
}

// if query.Streaming {
// 	// create data frame response.
// 	stream := data.NewFrame("stream")

// 	// add fields.
// 	stream.Fields = append(stream.Fields,
// 		data.NewField("time", nil, []time.Time{query.TimeRange.From, query.TimeRange.To}),
// 		data.NewField("values", nil, []int64{10, 20}),
// 	)

// 	channel := live.Channel{
// 		Scope:     live.ScopeDatasource,
// 		Namespace: pctx.DataSourceInstanceSettings.UID,
// 		Path:      "stream",
// 	}
// 	stream.SetMeta(&data.FrameMeta{Channel: channel.String()})
// }
