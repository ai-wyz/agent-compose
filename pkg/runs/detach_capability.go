package runs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/chaitin/agent-compose/pkg/execution"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// detachProbeTimeout bounds the capability probe exec; a slow guest must not
// stall the run start.
const detachProbeTimeout = 10 * time.Second

// detachCapabilityCache records, per sandbox, whether the guest runtime
// supports detached execution. The answer is a property of the guest image, so
// it is stable for a sandbox's lifetime.
type detachCapabilityCache struct {
	mu    sync.Mutex
	cache map[string]bool
}

func (c *detachCapabilityCache) get(sandboxID string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.cache[sandboxID]
	return value, ok
}

func (c *detachCapabilityCache) set(sandboxID string, value bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]bool{}
	}
	c.cache[sandboxID] = value
}

// detachSupported probes the guest runtime's `capabilities --json` and caches
// the answer per sandbox. A probe failure falls back to the legacy path.
func (c *Controller) detachSupported(ctx context.Context, runtime Runtime, sandbox *domain.Sandbox, vmState domain.VMState) bool {
	if c == nil || c.detachCache == nil || sandbox == nil {
		return false
	}
	sandboxID := sandbox.Summary.ID
	if value, ok := c.detachCache.get(sandboxID); ok {
		return value
	}
	probeCtx, cancel := context.WithTimeout(ctx, detachProbeTimeout)
	defer cancel()
	spec := execution.BuildDetachCapabilitiesSpec(c.config)
	spec.Env = execution.BuildSandboxExecEnv(probeCtx, c.config, sandbox, c.config.GuestHomePath)
	var stdout strings.Builder
	_, err := runtime.ExecStream(probeCtx, sandbox, vmState, spec, func(chunk domain.ExecChunk) {
		if chunk.Stream == domain.StdioStdout {
			stdout.WriteString(chunk.Text)
		}
	})
	caps, ok, _ := execution.ParseDetachCapabilities(stdout.String())
	supported := err == nil && ok && caps.Detach
	c.detachCache.set(sandboxID, supported)
	return supported
}

// persistDetachedRunFields records the detach decision on the run before the
// detached task starts, so a restarted daemon can re-attach it and re-arm its
// deadline.
func (c *Controller) persistDetachedRunFields(ctx context.Context, run domain.ProjectRunRecord, executionID string) error {
	if c.configDB == nil {
		return nil
	}
	run.ExecutionID = executionID
	run.Detach = true
	if deadline, ok := ctx.Deadline(); ok {
		run.Deadline = deadline
	}
	_, err := c.configDB.UpdateProjectRun(ctx, run)
	return err
}

// detachCursorFile returns the sidecar file that records the highest journal
// offset the daemon has consumed for a run. It lives beside the transcript so
// it survives a daemon restart on the same data volume.
func detachCursorFile(artifactsDir string) string {
	return filepath.Join(artifactsDir, "journal-cursor")
}

func readDetachCursor(artifactsDir string) uint64 {
	data, err := os.ReadFile(detachCursorFile(artifactsDir))
	if err != nil {
		return 0
	}
	offset, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return offset
}

func writeDetachCursor(artifactsDir string, offset uint64) error {
	path := detachCursorFile(artifactsDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(offset, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newDetachExecutionID returns a fresh execution id for a run that has none.
func newDetachExecutionID(run domain.ProjectRunRecord) string {
	if id := strings.TrimSpace(run.ExecutionID); id != "" {
		return id
	}
	return uuid.NewString()
}
