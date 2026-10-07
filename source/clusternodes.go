package main

import (
	"fmt"
	"sort"
	"strings"
)

// CanvasNode is one cluster node as the Topology drawing shows it (a parallelogram beside the gateway's circle), seen
// from the gateway it is drawn for: whether that node is serving it.
type CanvasNode struct {
	NodeID    string      `json:"node_id,omitempty"`
	Excluded  bool        `json:"excluded,omitempty"` // removed from this gateway (shared setting)
	Addr      string      `json:"addr"`
	Name      string      `json:"name"` // host name, else the address (the address when two nodes share a host name)
	Hostname  string      `json:"hostname,omitempty"`
	Self      bool        `json:"self"`   // the node being asked
	Status    string      `json:"status"` // ok | warn | bad | paused | idle, the same colours as every other shape
	Label     string      `json:"label"`  // the short word in the shape: serving, paused, not answering …
	Detail    string      `json:"detail"`
	Role      Role        `json:"role"`
	Host      *HostLoad   `json:"host,omitempty"`        // its CPU, memory and disk use
	Addrs     []NodeIface `json:"addrs,omitempty"`       // its Ethernet interfaces' IPv4 and IPv6 GUA addresses (tooltip)
	Strain    []string    `json:"strain,omitempty"`      // what is over the limit (the shape is yellow while there is anything)
	Paused    bool        `json:"node_paused,omitempty"` // the whole node is paused (Operate ▸ Node), as opposed to this gateway being paused there
	Reachable bool        `json:"reachable"`
	Version   string      `json:"version,omitempty"`
	VerDiff   bool        `json:"version_differs,omitempty"`
	Behind    bool        `json:"behind,omitempty"` // has not caught up with the primary's shared settings
	Updating  bool        `json:"updating,omitempty"`
	UpdateErr string      `json:"update_failed,omitempty"`
	LastSeen  int64       `json:"last_seen,omitempty"` // unix seconds
	Error     string      `json:"error,omitempty"`
}

var circleLabel = map[string]string{"ok": "serving", "warn": "degraded", "bad": "not serving", "paused": "paused", "idle": "starting"}

// canvasNodes lists the cluster for one gateway: this node first (its state is the gateway's own), then the others by
// address. selfStatus/selfDetail are the gateway's colour and reason on this node; selfPaused says the node itself is paused.
func (c *Cluster) canvasNodes(gid int, selfStatus, selfDetail string, selfPaused bool) []CanvasNode {
	return c.canvasNodesEx(gid, selfStatus, selfDetail, selfPaused, nil)
}

// canvasNodesEx is canvasNodes with the node IDs removed from the gateway: those nodes read "removed" whichever node is asked.
func (c *Cluster) canvasNodesEx(gid int, selfStatus, selfDetail string, selfPaused bool, excluded []string) []CanvasNode {
	if c == nil || !c.Enabled() {
		return nil
	}
	view := c.View()
	if len(view.Peers) < 2 {
		return nil // a single node is not a cluster: nothing to draw
	}
	var primaryRev uint64
	for _, p := range view.Peers {
		if p.IsPrimary || (p.Self && p.Role == RolePrimary) {
			primaryRev = p.SharedRev
		}
	}
	c.mu.Lock()
	infos := map[string]peerInfo{}
	for addr, pi := range c.info {
		if pi != nil {
			infos[addr] = *pi
		}
	}
	c.mu.Unlock()
	out := make([]CanvasNode, 0, len(view.Peers))
	for _, p := range view.Peers {
		n := CanvasNode{Addr: p.Addr, Name: p.Hostname, Self: p.Self, Role: p.Role, Reachable: p.Reachable, Version: p.Version,
			Updating: p.Updating, UpdateErr: p.UpdateFailed, Error: p.Error}
		n.Hostname = p.Hostname
		n.NodeID = p.NodeID
		n.Excluded = p.NodeID != "" && containsStr(excluded, p.NodeID)
		if n.Name == "" {
			n.Name = p.Addr
		}
		if p.IsPrimary {
			n.Role = RolePrimary
		}
		if !p.LastSeen.IsZero() && !p.Self {
			n.LastSeen = p.LastSeen.Unix()
		}
		n.VerDiff = !p.Self && p.Version != "" && p.Version != version()
		n.Behind = primaryRev > 0 && p.SharedRev < primaryRev
		if p.Self {
			n.Host = hostLoadPtr()
			n.Addrs = ethernetAddrs()
		}
		switch {
		case p.Self:
			n.Status, n.Detail, n.Paused = selfStatus, selfDetail, selfPaused
			n.Label = circleLabel[selfStatus]
			if n.Excluded {
				n.Status, n.Label = "paused", "removed"
			}
			if selfStatus == "idle" && strings.HasPrefix(selfDetail, "not running here") {
				// held back because this node has no address in the gateway's subnet (markOffnet): the other nodes see
				// "not serving" (amber) for it, so say the same here instead of "starting", which is for the DNS warm-up
				n.Status, n.Label = "warn", "not serving"
			}
			if selfStatus == "ok" && !selfPaused {
				// healthy: the same words as the other nodes' tooltips (what is said about a node should not depend on
				// which node you ask); in any other state the gateway's own reason on this node is the more useful text
				n.Detail = "Serving this gateway"
			}
		case !p.Reachable && !n.Excluded:
			n.Status, n.Label = "bad", "not answering"
			n.Detail = "Not answering"
			if !p.LastSeen.IsZero() {
				n.Detail += " since " + p.LastSeen.Format("15:04:05")
			}
			if p.Error != "" {
				n.Detail += ": " + p.Error
			}
		case n.Excluded:
			n.Status, n.Label, n.Detail = "paused", "removed", "Removed from this gateway: it does not serve it; the other nodes carry on"
			if !p.Reachable {
				n.Detail += " (and is not answering)"
			}
		default:
			pi := infos[p.Addr]
			n.Host = pi.Msg.Host
			n.Addrs = pi.Msg.Addrs
			var gs *GwState
			for i := range pi.Msg.Gateways {
				if pi.Msg.Gateways[i].GroupID == gid {
					gs = &pi.Msg.Gateways[i]
				}
			}
			n.Paused = pi.Msg.NodePaused
			switch {
			case pi.Msg.NodePaused:
				n.Status, n.Label, n.Detail = "paused", "paused", "This node is paused (Operate ▸ Node): it is not serving; the others carry on"
			case !pi.Msg.GwKnown:
				n.Status, n.Label, n.Detail = "idle", "online", "Running an older version that does not say what it serves"
			case gs == nil:
				n.Status, n.Label, n.Detail = "paused", "not serving", "This gateway is paused or not set up on that node"
			case !gs.Serving:
				n.Status, n.Label, n.Detail = "warn", "not serving", "That node is up but is not serving this gateway (yet)"
			case gs.Fresh:
				n.Status, n.Label, n.Detail = "ok", "serving", "Serving, but only for the last few seconds"
			default:
				n.Status, n.Label, n.Detail = "ok", "serving", "Serving this gateway"
			}
			// serving, but that node's own view of the gateway is not healthy (some DNS servers down, say):
			// show it the way this node's own shape shows it
			if n.Status == "ok" && (gs.Health == "warn" || gs.Health == "bad") {
				n.Status, n.Label = gs.Health, circleLabel[gs.Health]
				if gs.Health == "warn" {
					n.Label = "degraded"
				}
				n.Detail = "Serving, but its view of the gateway is " + circleLabel[gs.Health] + ": " + strings.TrimSpace(gs.HealthWhy)
			}
		}
		// a node whose CPU, memory or disk is over the limit is yellow (degraded) until all are back under it
		if n.Host != nil && (p.Self || p.Reachable) {
			if n.Strain = n.Host.Strained(true); len(n.Strain) > 0 {
				n.Status = "warn"
				n.Label = strings.Join(n.Host.Strained(false), " · ") // the mount is in the tooltip
				n.Detail = fmt.Sprintf("Over %.0f%%: %s (yellow until all are back under it). %s", hostStrainPct, strings.Join(n.Strain, ", "), n.Detail)
			}
		}
		out = append(out, n)
	}
	same := map[string]int{}
	for _, n := range out {
		same[n.Name]++
	}
	for i := range out {
		if same[out[i].Name] > 1 {
			out[i].Name = out[i].Addr // two nodes with one host name: told apart by address
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// markNodes adds the cluster's nodes to each gateway of the picture.
func (s *StatusServer) markNodes(groups []CanvasGateway) {
	if s.mg == nil || s.mg.cl == nil {
		return
	}
	for i := range groups {
		groups[i].Nodes = s.mg.cl.canvasNodesEx(groups[i].GroupID, groups[i].Status, groups[i].Detail, groups[i].NodePaused, groups[i].ExcludedNodes)
	}
}

func hostLoadPtr() *HostLoad {
	if l, ok := hostLoadFn(); ok {
		return &l
	}
	return nil
}
