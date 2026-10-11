package dynamic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

// DefaultPhaseTimeout bounds one phase round trip when plugins[].timeout is
// not set.
const DefaultPhaseTimeout = 5 * time.Second

// Loader is the plugin.DynamicLoader implementation: it resolves a plugin
// binary from plugins[].url (oci:// pull+cache, or file:// local path),
// spawns it as a subprocess and returns the plugin.Plugin adapter.
type Loader struct {
	// cacheDir is where OCI images are extracted (digest-addressed).
	cacheDir string
}

// New builds a Loader extracting into cacheDir.
func New(cacheDir string) *Loader {
	return &Loader{cacheDir: cacheDir}
}

// Install registers a Loader as the process-wide plugin dynamic loader.
// cacheDir empty falls back to <user cache dir>/ext-proc/plugins.
func Install(cacheDir string, logger *slog.Logger) {
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		cacheDir = filepath.Join(base, "ext-proc", "plugins")
	}
	plugin.SetDynamicLoader(New(cacheDir))
	logger.Info("dynamic plugin loader installed", slog.String("cacheDir", cacheDir))
}

// Load resolves the binary, spawns the subprocess and returns the adapter.
// Part of plugin.DynamicLoader.
func (l *Loader) Load(pc plugin.PluginConfig, cfg *gjson.Json, logger *slog.Logger) (plugin.Plugin, error) {
	timeout := DefaultPhaseTimeout
	if pc.Timeout != "" {
		parsed, err := time.ParseDuration(pc.Timeout)
		if err != nil {
			return nil, gerror.Wrapf(err, "plugin %s: invalid timeout %q", pc.Name, pc.Timeout)
		}
		if parsed > 0 {
			timeout = parsed
		}
	}

	binary, err := l.resolveBinary(pc.URL, logger)
	if err != nil {
		return nil, gerror.Wrapf(err, "plugin %s: resolve binary", pc.Name)
	}

	// The merged instance config travels as inline JSON; internal/pluginhost
	// parses it and hands the node to the plugin's Init inside the subprocess.
	cfgJSON := ""
	if cfg != nil {
		data, err := json.Marshal(cfg.Map())
		if err != nil {
			return nil, gerror.Wrapf(err, "plugin %s: marshal config", pc.Name)
		}
		cfgJSON = string(data)
	}

	proc, err := startProcess(pc.Name, binary,
		[]string{"-listen", "tcp:127.0.0.1:0"},
		[]string{envPluginConfig + "=" + cfgJSON},
		logger)
	if err != nil {
		return nil, gerror.Wrapf(err, "plugin %s: start subprocess", pc.Name)
	}

	logger.Info("dynamic plugin loaded",
		slog.String("plugin", pc.Name),
		slog.String("url", pc.URL),
		slog.String("binary", binary),
		slog.String("timeout", timeout.String()))
	return &subprocessPlugin{name: pc.Name, proc: proc, timeout: timeout, logger: logger}, nil
}

// resolveBinary maps a url to a local executable path.
func (l *Loader) resolveBinary(url string, logger *slog.Logger) (string, error) {
	switch {
	case strings.HasPrefix(url, "oci://"):
		return resolveOCI(strings.TrimPrefix(url, "oci://"), l.cacheDir, logger)
	case strings.HasPrefix(url, "file://"):
		p := strings.TrimPrefix(url, "file://")
		info, err := os.Stat(p)
		if err != nil {
			return "", gerror.Wrapf(err, "plugin binary %q not found", p)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", gerror.Newf("plugin binary %q must be an executable file", p)
		}
		return p, nil
	default:
		return "", gerror.Newf("unsupported dynamic plugin url %q (want oci://... or file:///abs/path)", url)
	}
}

// pluginBinaryName is the conventional executable name inside an OCI plugin
// image when no explicit annotation is present.
const pluginBinaryName = "plugin"

// findExecutable picks the plugin binary in an extracted image root: the
// explicit annotation wins, then a conventional "plugin" file, then a single
// executable file in the root.
func findExecutable(root string, execPath string) (string, error) {
	if execPath != "" {
		p := filepath.Join(root, filepath.FromSlash(execPath))
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("annotated executable %q not found in image", execPath)
	}
	conventional := filepath.Join(root, pluginBinaryName)
	if info, err := os.Stat(conventional); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return conventional, nil
	}
	var candidates []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := e.Info(); err == nil && info.Mode()&0o111 != 0 {
			candidates = append(candidates, filepath.Join(root, e.Name()))
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return "", fmt.Errorf("cannot pick the plugin executable in %s (found %d candidates; set the image annotation %q)",
		root, len(candidates), ociExecAnnotation)
}
