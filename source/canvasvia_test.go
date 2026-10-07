package main

import "testing"

func viaNodes(self string, others ...CanvasNode) []CanvasNode {
	return append([]CanvasNode{{Name: "me", Addr: "10.0.0.1:1", Self: true, Gw: self, Reachable: true}}, others...)
}

func peer(name, gw string) CanvasNode {
	return CanvasNode{Name: name, Addr: "10.0.0." + name + ":1", Gw: gw, Reachable: true}
}

// Only a node that does not serve the gateway takes the picture of one that does, best serving node first.
func TestServingPeersAreAskedOnlyWhenThisNodeDoesNotServe(t *testing.T) {
	others := []CanvasNode{peer("5", "degraded"), peer("4", "ok"), peer("3", "notserving"), peer("2", "ok")}
	for _, self := range []string{"ok", "degraded", "bad", "starting", ""} {
		if l := servingPeers(CanvasGateway{Nodes: viaNodes(self, others...)}); len(l) != 0 {
			t.Errorf("this node is %q: %d nodes asked", self, len(l))
		}
	}
	for _, self := range []string{"notserving", "paused", "removed"} {
		l := servingPeers(CanvasGateway{Nodes: viaNodes(self, others...)})
		if len(l) != 3 || l[0].Name != "2" || l[1].Name != "4" || l[2].Name != "5" {
			t.Errorf("this node is %q: %+v", self, l)
		}
	}
	gone := peer("6", "ok")
	gone.Reachable = false
	removed := peer("7", "ok")
	removed.Excluded = true
	if l := servingPeers(CanvasGateway{Nodes: viaNodes("notserving", gone, removed, peer("8", "paused"), peer("9", "down"))}); len(l) != 0 {
		t.Errorf("nodes that cannot say were asked: %+v", l)
	}
}

func TestTakeFromKeepsThisNodesOwnEntry(t *testing.T) {
	mine := CanvasGateway{GroupID: 1, Name: "x", Status: "idle", Detail: "not running here", Nodes: viaNodes("notserving"), Servers: []CanvasServer{}, Paused: true, NodePaused: true}
	theirs := CanvasGateway{GroupID: 1, Status: "ok", Detail: "running and answering", Servers: []CanvasServer{{Addr: "192.0.2.1:53", Status: "ok"}}, Families: []CanvasFamily{{AF: "v4", Status: "ok"}}}
	mine.takeFrom(theirs, peer("2", "ok"))
	if mine.Status != "ok" || mine.Detail != "running and answering" || len(mine.Servers) != 1 || len(mine.Families) != 1 {
		t.Fatalf("not taken: %+v", mine)
	}
	if mine.Via != "2" || mine.ViaAddr != "10.0.0.2:1" || len(mine.Nodes) != 1 || mine.Name != "x" || !mine.Paused {
		t.Fatalf("this node's own part changed: %+v", mine)
	}
}
