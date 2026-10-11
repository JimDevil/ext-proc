package processor

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// closeRecorder records that its Close ran.
type closeRecorder struct {
	plugin.BasePlugin
	suffix string
	closed atomic.Bool
}

func (p *closeRecorder) Name() string { return "retire-" + p.suffix }
func (p *closeRecorder) Close() error { p.closed.Store(true); return nil }

func TestSwapChainRetiresOldTableAfterTTL(t *testing.T) {
	var a, b closeRecorder
	a.suffix, b.suffix = "a", "b"
	plugin.Register("retire-a", func() plugin.Plugin { return &a })
	plugin.Register("retire-b", func() plugin.Plugin { return &b })

	cfgA := plugin.ChainConfig{Plugins: []plugin.PluginConfig{{Name: "retire-a", Enabled: true, Order: 10}}}
	cfgB := plugin.ChainConfig{Plugins: []plugin.PluginConfig{{Name: "retire-b", Enabled: true, Order: 10}}}

	p := New("test", 50*time.Millisecond, testLogger)
	table1, err := plugin.BuildRouteTable(cfgA, nil, testLogger)
	if err != nil {
		t.Fatalf("build table1: %v", err)
	}
	table2, err := plugin.BuildRouteTable(cfgB, nil, testLogger)
	if err != nil {
		t.Fatalf("build table2: %v", err)
	}

	p.SwapChain(table1)
	p.SwapChain(table2) // table1 retired now

	// The retired table's instances must be closed after the TTL.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.closed.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retired table was not closed within the deadline")
}

func TestSwapChainWithoutTTLKeepsOldTable(t *testing.T) {
	var a closeRecorder
	a.suffix = "keep"
	plugin.Register("retire-keep", func() plugin.Plugin { return &a })

	cfg := plugin.ChainConfig{Plugins: []plugin.PluginConfig{{Name: "retire-keep", Enabled: true, Order: 10}}}
	table1, err := plugin.BuildRouteTable(cfg, nil, testLogger)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	table2, err := plugin.BuildRouteTable(cfg, nil, testLogger)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	p := New("test", 0, testLogger) // 0 disables retiring closes
	p.SwapChain(table1)
	p.SwapChain(table2)

	time.Sleep(80 * time.Millisecond)
	if a.closed.Load() {
		t.Fatal("with TTL disabled the old table must not be closed")
	}
}
