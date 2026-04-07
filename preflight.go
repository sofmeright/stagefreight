// Package runner implements execution substrate introspection.
//
// Three-layer model (mandatory):
//   - Layer 1 — SubstrateFacts: unconditional raw measurements (no mode params, no policy)
//   - Layer 2 — []Finding: policy evaluation (same facts, different severity per mode)
//   - Layer 3 — HealthGrade: deterministic reducer from findings
//
// This package has no knowledge of CI runners, pipeline phases, or output rendering.
// It returns structured data; callers own rendering and policy wiring.
package runner

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ExecutionEngine identifies the CI orchestrator or execution context.
type ExecutionEngine string

const (
	EngineGitLab       ExecutionEngine = "gitlab"
	EngineGitHub       ExecutionEngine = "github"
	EngineForgejo      ExecutionEngine = "forgejo"
	EngineGitea        ExecutionEngine = "gitea"
	EngineLocal        ExecutionEngine = "local"
	EngineStageFreight ExecutionEngine = "stagefreight"
	EngineUnknown      ExecutionEngine = "unknown"
)

// ExecutionIdentity holds engine-specific session fields.
type ExecutionIdentity struct {
	Name       string `json:"name,omitempty"`        // CI_RUNNER_DESCRIPTION / RUNNER_NAME
	PipelineID string `json:"pipeline_id,omitempty"` // CI_PIPELINE_ID / GITHUB_RUN_ID
	JobID      string `json:"job_id,omitempty"`      // CI_JOB_ID / GITHUB_JOB
	Workflow   string `json:"workflow,omitempty"`    // GITHUB_WORKFLOW (GitHub only)
	Controller string `json:"controller,omitempty"` // future: SF daemon controller
	Satellite  string `json:"satellite,omitempty"`  // future: SF satellite node
}

// SubstrateFacts is Layer 1: raw substrate measurements, always collected unconditionally.
// Named "Substrate" not "Runner" because these facts describe the execution substrate
// regardless of whether it is a CI runner, local machine, or SF daemon node.
// No mode parameters. No policy. No severity. Just facts.
type SubstrateFacts struct {
	DockerSocket         string  `json:"docker_socket"`       // path if found, "" if absent
	DockerAvailable      bool    `json:"docker_available"`
	DindDetected         bool    `json:"dind_detected"`
	BuildKitAvailable    bool    `json:"buildkit_available"`  // = DockerAvailable (modern Docker ships BuildKit)
	BuildxAvailable      bool    `json:"buildx_available"`
	DiskFreeMB           int64   `json:"disk_free_mb"`        // .stagefreight filesystem
	TmpFreeMB            int64   `json:"tmp_free_mb"`
	MemAvailableMB       int64   `json:"mem_available_mb"`    // -1 if unsupported (non-Linux)
	CPULoadAvg1          float64 `json:"cpu_load_avg1"`       // -1 if unsupported
	StagefreightWritable bool    `json:"stagefreight_writable"`
	WorkdirReadable      bool    `json:"workdir_readable"`
	InodePctFree         int     `json:"inode_pct_free"`      // -1 if unsupported
}

// Finding is Layer 2: policy applied to facts.
type Finding struct {
	ID       string `json:"id"`
	Status   string `json:"status"`          // "ok" | "warn" | "fail"
	Detail   string `json:"detail,omitempty"`
	Severity string `json:"severity"`        // "hard_fail" | "warn" | "info"
}

// HealthGrade is Layer 3: derived from findings.
type HealthGrade string

const (
	Healthy   HealthGrade = "healthy"
	Degraded  HealthGrade = "degraded"
	Unhealthy HealthGrade = "unhealthy"
)

// ExecutionReport is the complete execution introspection result.
// JSON field names use "runner" prefix in cistate for compatibility.
type ExecutionReport struct {
	Engine       ExecutionEngine `json:"engine"`
	InvocationID string          `json:"invocation_id"` // universal correlation key
	Identity     ExecutionIdentity  `json:"identity"`
	Facts        SubstrateFacts  `json:"facts"`
	Findings     []Finding       `json:"findings"`
	Health       HealthGrade     `json:"health"`
}

// Options controls Layer 2 policy only — never Layer 1 fact collection.
type Options struct {
	DockerRequired bool  // true = docker absence is hard_fail; false = info
	IsCrucible     bool  // doubles disk/memory warn thresholds
	DiskWarnMB     int64 // 0 = default (2048)
	DiskFailMB     int64 // 0 = default (512)
	MemWarnMB      int64 // 0 = default (512); IsCrucible doubles to 1024
}

// DetectEngine identifies the CI orchestrator from environment variables.
func DetectEngine() ExecutionEngine {
	switch {
	case os.Getenv("GITLAB_CI") == "true":
		return EngineGitLab
	case os.Getenv("GITHUB_ACTIONS") == "true":
		return EngineGitHub
	case os.Getenv("FORGEJO_ACTIONS") == "true":
		return EngineForgejo
	case os.Getenv("GITEA_ACTIONS") == "true":
		return EngineGitea
	case os.Getenv("STAGEFREIGHT_DAEMON") == "true":
		return EngineStageFreight
	default:
		return EngineLocal
	}
}

// ExtractIdentity reads engine-specific session fields from environment variables.
func ExtractIdentity(engine ExecutionEngine) ExecutionIdentity {
	switch engine {
	case EngineGitLab:
		return ExecutionIdentity{
			Name:       os.Getenv("CI_RUNNER_DESCRIPTION"),
			PipelineID: os.Getenv("CI_PIPELINE_ID"),
			JobID:      os.Getenv("CI_JOB_ID"),
		}
	case EngineGitHub:
		return ExecutionIdentity{
			Name:       os.Getenv("RUNNER_NAME"),
			PipelineID: os.Getenv("GITHUB_RUN_ID"),
			JobID:      os.Getenv("GITHUB_JOB"),
			Workflow:   os.Getenv("GITHUB_WORKFLOW"),
		}
	case EngineForgejo:
		return ExecutionIdentity{
			Name:       os.Getenv("RUNNER_NAME"),
			PipelineID: os.Getenv("FORGEJO_PIPELINE_ID"),
		}
	case EngineGitea:
		return ExecutionIdentity{
			Name:       os.Getenv("RUNNER_NAME"),
			PipelineID: os.Getenv("GITEA_PIPELINE_ID"),
		}
	default:
		return ExecutionIdentity{}
	}
}

// ResolveInvocationID returns a universal correlation ID for this run.
// For CI engines, this is the pipeline/run ID. For local runs, a generated 12-char hex ID.
func ResolveInvocationID(engine ExecutionEngine, identity ExecutionIdentity) string {
	switch engine {
	case EngineGitLab, EngineGitHub, EngineForgejo, EngineGitea:
		if identity.PipelineID != "" {
			return identity.PipelineID
		}
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "000000000000"
	}
	return fmt.Sprintf("%x", b)
}

// CollectFacts gathers raw substrate measurements unconditionally.
// No mode parameters. No policy. No severity. Just facts.
func CollectFacts(rootDir string) SubstrateFacts {
	facts := SubstrateFacts{
		MemAvailableMB: -1,
		CPULoadAvg1:    -1,
		InodePctFree:   -1,
	}

	// ── Docker detection (always runs — unconditional by design) ──────────────
	//
	// Docker may be accessible via a local unix socket OR a remote TCP endpoint
	// (DinD service containers, remote daemons, etc.).
	// Detection strategy differs by case:
	//   - TCP (DOCKER_HOST=tcp://...): no local socket exists; prove via docker info
	//   - Unix socket: stat + open for permission check
	//
	// Capability is proven, not inferred from socket presence alone.
	dockerHostEnv := os.Getenv("DOCKER_HOST")

	if strings.HasPrefix(dockerHostEnv, "tcp://") {
		// Remote/service-container Docker daemon.
		// Socket probing is not applicable — prove by running docker info.
		facts.DockerSocket = dockerHostEnv
		if probeDockerDaemon() {
			facts.DockerAvailable = true
		}
	} else {
		socketPath := "/var/run/docker.sock"
		if strings.HasPrefix(dockerHostEnv, "unix://") {
			socketPath = strings.TrimPrefix(dockerHostEnv, "unix://")
		}
		if _, err := os.Stat(socketPath); err == nil {
			facts.DockerSocket = socketPath
			if f, err := os.Open(socketPath); err == nil {
				f.Close()
				facts.DockerAvailable = true
			}
		}
	}

	// BuildKit availability: Docker ships BuildKit, but BuildKit can also be available
	// standalone (pure buildkitd, remote builder, buildkitd sidecar without Docker).
	// Probe independently so the two facts are not conflated.
	facts.BuildKitAvailable = facts.DockerAvailable || probeBuildKit()

	// Buildx: CLI plugin or standalone binary
	if home := os.Getenv("HOME"); home != "" {
		if _, err := os.Stat(filepath.Join(home, ".docker/cli-plugins/docker-buildx")); err == nil {
			facts.BuildxAvailable = true
		}
	}
	if !facts.BuildxAvailable {
		if _, err := exec.LookPath("docker-buildx"); err == nil {
			facts.BuildxAvailable = true
		}
	}

	// DinD detection: inside container + local socket accessible.
	// TCP docker endpoints are service-container docker, not socket-mount DinD.
	if facts.DockerAvailable {
		if !strings.HasPrefix(dockerHostEnv, "tcp://") {
			inContainer := false
			if _, err := os.Stat("/.dockerenv"); err == nil {
				inContainer = true
			}
			if !inContainer {
				if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
					s := string(data)
					if strings.Contains(s, "docker") || strings.Contains(s, "containerd") {
						inContainer = true
					}
				}
			}
			facts.DindDetected = inContainer
		}
	}

	// ── Disk ──────────────────────────────────────────────────────────────────
	sfDir := filepath.Join(rootDir, ".stagefreight")
	_ = os.MkdirAll(sfDir, 0o755)

	statTarget := sfDir
	if _, err := os.Stat(sfDir); err != nil {
		statTarget = rootDir
	}
	var diskStat syscall.Statfs_t
	if err := syscall.Statfs(statTarget, &diskStat); err == nil {
		facts.DiskFreeMB = int64(diskStat.Bavail) * int64(diskStat.Bsize) / (1024 * 1024)
		if diskStat.Files > 0 {
			facts.InodePctFree = int(uint64(100) * uint64(diskStat.Ffree) / uint64(diskStat.Files))
		}
	}

	var tmpStat syscall.Statfs_t
	if err := syscall.Statfs("/tmp", &tmpStat); err == nil {
		facts.TmpFreeMB = int64(tmpStat.Bavail) * int64(tmpStat.Bsize) / (1024 * 1024)
	}

	// ── Memory (Linux only) ───────────────────────────────────────────────────
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "MemAvailable:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
							facts.MemAvailableMB = kb / 1024
						}
					}
					break
				}
			}
		}
	}

	// ── CPU load (Linux only) ─────────────────────────────────────────────────
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) >= 1 {
				if load, err := strconv.ParseFloat(fields[0], 64); err == nil {
					facts.CPULoadAvg1 = load
				}
			}
		}
	}

	// ── Writability ───────────────────────────────────────────────────────────
	if f, err := os.CreateTemp(sfDir, ".sf-write-test-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		facts.StagefreightWritable = true
	}
	if _, err := os.Stat(rootDir); err == nil {
		facts.WorkdirReadable = true
	}

	return facts
}

// EvaluateHealth applies policy to SubstrateFacts and returns findings + grade.
// Options controls Layer 2 only — CollectFacts is unconditional and unaffected.
func EvaluateHealth(facts SubstrateFacts, opts Options) ([]Finding, HealthGrade) {
	diskWarn := int64(2048)
	diskFail := int64(512)
	memWarn := int64(512) // warn below 512 MB available; IsCrucible doubles to 1024 MB

	if opts.DiskWarnMB > 0 {
		diskWarn = opts.DiskWarnMB
	}
	if opts.DiskFailMB > 0 {
		diskFail = opts.DiskFailMB
	}
	if opts.MemWarnMB > 0 {
		memWarn = opts.MemWarnMB
	}
	if opts.IsCrucible {
		diskWarn *= 2
		memWarn *= 2
	}

	var findings []Finding

	// workdir readable — always hard_fail
	findings = append(findings, evaluateBool("workdir_readable", facts.WorkdirReadable,
		"hard_fail", "working directory is not readable"))

	// stagefreight writable — always hard_fail
	findings = append(findings, evaluateBool("stagefreight_writable", facts.StagefreightWritable,
		"hard_fail", ".stagefreight is not writable"))

	// disk critical — always hard_fail
	if facts.DiskFreeMB < diskFail {
		findings = append(findings, Finding{
			ID:       "disk_critical",
			Status:   "fail",
			Detail:   fmt.Sprintf("%s free — below %s minimum", fmtMB(facts.DiskFreeMB), fmtMB(diskFail)),
			Severity: "hard_fail",
		})
	} else {
		findings = append(findings, Finding{ID: "disk_critical", Status: "ok", Severity: "hard_fail"})
	}

	// docker absent — hard_fail when DockerRequired, info otherwise
	dockerSeverity := "info"
	if opts.DockerRequired {
		dockerSeverity = "hard_fail"
	}
	if !facts.DockerAvailable {
		findings = append(findings, Finding{
			ID:       "docker_absent",
			Status:   "fail",
			Detail:   "docker socket not available",
			Severity: dockerSeverity,
		})
	} else {
		findings = append(findings, Finding{ID: "docker_absent", Status: "ok", Severity: dockerSeverity})
	}

	// disk low — warn
	if facts.DiskFreeMB >= diskFail && facts.DiskFreeMB < diskWarn {
		findings = append(findings, Finding{
			ID:       "disk_low",
			Status:   "warn",
			Detail:   fmt.Sprintf("%s free — below %s recommended", fmtMB(facts.DiskFreeMB), fmtMB(diskWarn)),
			Severity: "warn",
		})
	} else if facts.DiskFreeMB >= diskWarn {
		findings = append(findings, Finding{ID: "disk_low", Status: "ok", Severity: "warn"})
	}

	// tmp low — warn
	if facts.TmpFreeMB < 1024 {
		findings = append(findings, Finding{
			ID:       "tmp_low",
			Status:   "warn",
			Detail:   fmt.Sprintf("%s free in /tmp — below 1 GB recommended", fmtMB(facts.TmpFreeMB)),
			Severity: "warn",
		})
	} else {
		findings = append(findings, Finding{ID: "tmp_low", Status: "ok", Severity: "warn"})
	}

	// memory low — warn (advisory only; container envs often misreport)
	if facts.MemAvailableMB >= 0 && facts.MemAvailableMB < memWarn {
		findings = append(findings, Finding{
			ID:       "memory_low",
			Status:   "warn",
			Detail:   fmt.Sprintf("%s available — below %s recommended", fmtMB(facts.MemAvailableMB), fmtMB(memWarn)),
			Severity: "warn",
		})
	} else if facts.MemAvailableMB >= 0 {
		findings = append(findings, Finding{ID: "memory_low", Status: "ok", Severity: "warn"})
	}

	// inode low — warn
	if facts.InodePctFree >= 0 && facts.InodePctFree < 10 {
		findings = append(findings, Finding{
			ID:       "inode_low",
			Status:   "warn",
			Detail:   fmt.Sprintf("%d%% inodes free — below 10%% threshold", facts.InodePctFree),
			Severity: "warn",
		})
	} else if facts.InodePctFree >= 0 {
		findings = append(findings, Finding{ID: "inode_low", Status: "ok", Severity: "warn"})
	}

	return findings, gradeFromFindings(findings)
}

// gradeFromFindings reduces findings to a HealthGrade.
// Hard failures dominate, then warnings, then healthy.
func gradeFromFindings(findings []Finding) HealthGrade {
	for _, f := range findings {
		if f.Status == "fail" && f.Severity == "hard_fail" {
			return Unhealthy
		}
	}
	for _, f := range findings {
		if f.Status == "warn" || (f.Status == "fail" && f.Severity == "warn") {
			return Degraded
		}
	}
	return Healthy
}

// Run collects substrate facts, evaluates health policy, and assembles an ExecutionReport.
// This is the primary entry point.
func Run(rootDir string, opts Options) ExecutionReport {
	engine := DetectEngine()
	identity := ExtractIdentity(engine)
	invocationID := ResolveInvocationID(engine, identity)
	facts := CollectFacts(rootDir)
	findings, health := EvaluateHealth(facts, opts)

	return ExecutionReport{
		Engine:       engine,
		InvocationID: invocationID,
		Identity:     identity,
		Facts:        facts,
		Findings:     findings,
		Health:       health,
	}
}

// evaluateBool produces a finding for a boolean fact.
func evaluateBool(id string, ok bool, failSeverity, failDetail string) Finding {
	if ok {
		return Finding{ID: id, Status: "ok", Severity: failSeverity}
	}
	return Finding{ID: id, Status: "fail", Detail: failDetail, Severity: failSeverity}
}

func fmtMB(mb int64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// probeDockerDaemon runs docker info to verify daemon reachability.
// Used for TCP docker endpoints where socket-based detection is not applicable.
// Respects DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH from environment.
// Short timeout (2s) prevents preflight from blocking CI jobs on unreachable daemons.
func probeDockerDaemon() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// probeBuildKit runs buildctl to verify buildkitd reachability.
// Respects BUILDKIT_HOST from environment — covers standalone buildkitd,
// remote builders, and sidecar buildkitd containers (tcp://buildkitd:1234 etc.).
// Short timeout (2s) prevents preflight from blocking CI jobs on unreachable daemons.
func probeBuildKit() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "buildctl", "debug", "workers")
	return cmd.Run() == nil
}
