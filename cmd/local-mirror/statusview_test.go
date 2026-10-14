package main

import (
	"strings"
	"testing"

	"local-mirror/internal/status"
	"local-mirror/pkg/termstyle"
)

// TestListenRowShowsDegraded 某个地址族停止 accept 时，这一行必须显式说出来。
// 上次这个故障之所以难查，就是因为所有可观测信号都显示正常
func TestListenRowShowsDegraded(t *testing.T) {
	p := termstyle.Palette{} // 无色，便于断言文本
	snap := &status.Snapshot{Listen: []status.ListenFamily{
		{Family: "IPv4", Addr: "0.0.0.0:52345", Live: true},
		{Family: "IPv6", Addr: "[::]:52345", Live: false},
	}}

	got := listenRow(snap, p, true)
	for _, want := range []string{"IPv4", "IPv6", "not accepting", "bound but unserved"} {
		if !strings.Contains(got, want) {
			t.Errorf("listenRow() = %q, missing %q", got, want)
		}
	}
}

// TestListenRowHealthyAndDialOut 双栈健康时不报警；拨出侧无监听则不显示该行
func TestListenRowHealthyAndDialOut(t *testing.T) {
	p := termstyle.Palette{}
	healthy := &status.Snapshot{Listen: []status.ListenFamily{
		{Family: "IPv4", Addr: "0.0.0.0:52345", Live: true},
		{Family: "IPv6", Addr: "[::]:52345", Live: true},
	}}
	got := listenRow(healthy, p, true)
	if strings.Contains(got, "not accepting") || strings.Contains(got, "unserved") {
		t.Errorf("healthy dual stack should not warn: %q", got)
	}
	if !strings.Contains(got, "IPv4") || !strings.Contains(got, "IPv6") {
		t.Errorf("healthy dual stack should list both families: %q", got)
	}
	if got := listenRow(&status.Snapshot{}, p, true); got != "" {
		t.Errorf("a dialing process has no listen row, got %q", got)
	}
}
