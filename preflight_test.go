package runner

import (
	"os"
	"strings"
	"testing"
)

// ── Engine detection ─────────────────────────────────────────────────────────

func TestDetectEngine_GitLab(t *testing.T) {
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("GITHUB_ACTIONS", "")
	if got := DetectEngine(); got != EngineGitLab {
		t.Errorf("want %q, got %q", EngineGitLab, got)
	}
}

func TestDetectEngine_GitHub(t *testing.T) {
	t.Setenv("GITLAB_CI", "")
	t.Setenv("GITHUB_ACTIONS", "true")
	if got := DetectEngine(); got != EngineGitHub {
		t.Errorf("want %q, got %q", EngineGitHub, got)
	}
}

func TestDetectEngine_Local(t *testing.T) {
	t.Setenv("GITLAB_CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("FORGEJO_ACTIONS", "")
	t.Setenv("GITEA_ACTIONS", "")
	t.Setenv("STAGEFREIGHT_DAEMON", "")
	if got := DetectEngine(); got != EngineLocal {
		t.Errorf("want %q, got %q", EngineLocal, got)
	}
}

// ── InvocationID ─────────────────────────────────────────────────────────────

func TestInvocationID_LocalGeneratesHex(t *testing.T) {
	id := ResolveInvocationID(EngineLocal, ExecutionIdentity{})
	if len(id) == 0 {
		t.Fatal("expected non-empty invocation ID for local engine")
	}
	// Must be 12 hex chars (6 bytes).
	if len(id) != 12 {
		t.Errorf("want 12 hex chars, got %d: %q", len(id), id)
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char %q in invocation ID %q", c, id)
		}
	}
}

func TestInvocationID_LocalUniqueAcrossCalls(t *testing.T) {
	a := ResolveInvocationID(EngineLocal, ExecutionIdentity{})
	b := ResolveInvocationID(EngineLocal, ExecutionIdentity{})
	// Two separate calls should (overwhelmingly) produce different IDs.
	// Collision probability is 1/2^48 — effectively impossible in tests.
	if a == b {
		t.Errorf("two local invocation IDs should not collide: both %q", a)
	}
}

func TestInvocationID_GitLabUsesPipelineID(t *testing.T) {
	id := ResolveInvocationID(EngineGitLab, ExecutionIdentity{PipelineID: "9876"})
	if id != "9876" {
		t.Errorf("want %q, got %q", "9876", id)
	}
}

// ── Fact collection ──────────────────────────────────────────────────────────

// TestCollectSubstrateFacts_DockerAlwaysProbed verifies that docker socket
// probing runs unconditionally — CollectFacts takes no mode parameters.
// This test doesn't assert docker availability (varies by environment);
// it asserts the function completes and populates struct fields correctly.
func TestCollectSubstrateFacts_DockerAlwaysProbed(t *testing.T) {
	dir := t.TempDir()
	facts := CollectFacts(dir)

	// WorkdirReadable must be true — we just created the dir.
	if !facts.WorkdirReadable {
		t.Error("WorkdirReadable should be true for a valid temp dir")
	}

	// StagefreightWritable must be true — .stagefreight is created inside dir.
	if !facts.StagefreightWritable {
		t.Error("StagefreightWritable should be true for a writable temp dir")
	}

	// DiskFreeMB must be a non-negative value — always populated on Linux.
	if facts.DiskFreeMB < 0 {
		t.Errorf("DiskFreeMB should be >= 0, got %d", facts.DiskFreeMB)
	}

	// DockerAvailable and DockerSocket are correlated.
	if facts.DockerAvailable && facts.DockerSocket == "" {
		t.Error("DockerAvailable=true but DockerSocket is empty")
	}
	if !facts.DockerAvailable && facts.DockerSocket != "" {
		t.Error("DockerAvailable=false but DockerSocket is non-empty")
	}

	// BuildKitAvailable is independent of DockerAvailable — BuildKit can be available
	// via standalone buildkitd even when Docker is absent.
	// When Docker IS available, BuildKit must also be available (ships with Docker).
	if facts.DockerAvailable && !facts.BuildKitAvailable {
		t.Error("BuildKitAvailable must be true when DockerAvailable is true")
	}
}

// ── Health evaluation ─────────────────────────────────────────────────────────

func TestEvaluateHealth_DockerAbsent_Required(t *testing.T) {
	facts := SubstrateFacts{
		DockerAvailable:      false,
		WorkdirReadable:      true,
		StagefreightWritable: true,
		DiskFreeMB:           10000,
		TmpFreeMB:            5000,
		MemAvailableMB:       -1, // unsupported
		InodePctFree:         -1,
	}
	_, grade := EvaluateHealth(facts, Options{DockerRequired: true})
	if grade != Unhealthy {
		t.Errorf("want Unhealthy when docker absent and DockerRequired=true, got %q", grade)
	}
}

func TestEvaluateHealth_DockerAbsent_NotRequired(t *testing.T) {
	facts := SubstrateFacts{
		DockerAvailable:      false,
		WorkdirReadable:      true,
		StagefreightWritable: true,
		DiskFreeMB:           10000,
		TmpFreeMB:            5000,
		MemAvailableMB:       -1,
		InodePctFree:         -1,
	}
	findings, grade := EvaluateHealth(facts, Options{DockerRequired: false})
	if grade != Healthy {
		t.Errorf("want Healthy when docker absent and DockerRequired=false, got %q", grade)
	}
	// docker_absent finding must exist with severity "info" (not hard_fail).
	var dockerFinding *Finding
	for i := range findings {
		if findings[i].ID == "docker_absent" {
			dockerFinding = &findings[i]
			break
		}
	}
	if dockerFinding == nil {
		t.Fatal("expected docker_absent finding, none found")
	}
	if dockerFinding.Severity != "info" {
		t.Errorf("docker_absent severity: want %q, got %q", "info", dockerFinding.Severity)
	}
}

func TestEvaluateHealth_DegradedMemory(t *testing.T) {
	facts := SubstrateFacts{
		WorkdirReadable:      true,
		StagefreightWritable: true,
		DiskFreeMB:           10000,
		TmpFreeMB:            5000,
		MemAvailableMB:       256, // below default 512 MB warn threshold
		InodePctFree:         -1,
	}
	_, grade := EvaluateHealth(facts, Options{})
	if grade != Degraded {
		t.Errorf("want Degraded for low memory, got %q", grade)
	}
}

func TestEvaluateHealth_UnhealthyNotWritable(t *testing.T) {
	facts := SubstrateFacts{
		WorkdirReadable:      true,
		StagefreightWritable: false, // hard_fail condition
		DiskFreeMB:           10000,
		TmpFreeMB:            5000,
		MemAvailableMB:       -1,
		InodePctFree:         -1,
	}
	_, grade := EvaluateHealth(facts, Options{})
	if grade != Unhealthy {
		t.Errorf("want Unhealthy when StagefreightWritable=false, got %q", grade)
	}
}

func TestEvaluateHealth_CrucibleTighterThresholds(t *testing.T) {
	// Default disk warn is 2048 MB. Crucible doubles it to 4096.
	// A value of 3000 MB passes normal but fails crucible warn threshold.
	facts := SubstrateFacts{
		WorkdirReadable:      true,
		StagefreightWritable: true,
		DiskFreeMB:           3000,  // above 2048 (normal warn) but below 4096 (crucible warn)
		TmpFreeMB:            5000,
		MemAvailableMB:       -1,
		InodePctFree:         -1,
	}

	_, normalGrade := EvaluateHealth(facts, Options{IsCrucible: false})
	if normalGrade != Healthy {
		t.Errorf("normal mode: want Healthy at 3000 MB disk, got %q", normalGrade)
	}

	_, crucibleGrade := EvaluateHealth(facts, Options{IsCrucible: true})
	if crucibleGrade != Degraded {
		t.Errorf("crucible mode: want Degraded at 3000 MB disk (threshold 4096), got %q", crucibleGrade)
	}
}

// ── Grade reducer ─────────────────────────────────────────────────────────────

func TestGradeReducer_HardFailDominates(t *testing.T) {
	findings := []Finding{
		{ID: "memory_low", Status: "warn", Severity: "warn"},
		{ID: "workdir_readable", Status: "fail", Severity: "hard_fail"},
		{ID: "docker_absent", Status: "ok", Severity: "info"},
	}
	if got := gradeFromFindings(findings); got != Unhealthy {
		t.Errorf("hard_fail finding must dominate: want Unhealthy, got %q", got)
	}
}

func TestGradeReducer_WarnWithoutHardFail(t *testing.T) {
	findings := []Finding{
		{ID: "disk_low", Status: "warn", Severity: "warn"},
		{ID: "workdir_readable", Status: "ok", Severity: "hard_fail"},
	}
	if got := gradeFromFindings(findings); got != Degraded {
		t.Errorf("warn finding without hard_fail: want Degraded, got %q", got)
	}
}

func TestGradeReducer_AllOk(t *testing.T) {
	findings := []Finding{
		{ID: "workdir_readable", Status: "ok", Severity: "hard_fail"},
		{ID: "disk_low", Status: "ok", Severity: "warn"},
	}
	if got := gradeFromFindings(findings); got != Healthy {
		t.Errorf("all-ok findings: want Healthy, got %q", got)
	}
}

// ── DinD detection ────────────────────────────────────────────────────────────

func TestDindDetection_RemoteDocker(t *testing.T) {
	// When DOCKER_HOST is tcp://, DinD must be false regardless of socket availability.
	t.Setenv("DOCKER_HOST", "tcp://172.17.0.1:2375")
	dir := t.TempDir()
	facts := CollectFacts(dir)
	if facts.DindDetected {
		t.Error("DindDetected should be false when DOCKER_HOST is tcp://")
	}
}

// ── Run (integration) ─────────────────────────────────────────────────────────

func TestRun_PopulatesEngineAndInvocationID(t *testing.T) {
	t.Setenv("GITLAB_CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("FORGEJO_ACTIONS", "")
	t.Setenv("GITEA_ACTIONS", "")
	t.Setenv("STAGEFREIGHT_DAEMON", "")

	dir := t.TempDir()
	report := Run(dir, Options{})

	if report.Engine != EngineLocal {
		t.Errorf("want engine %q, got %q", EngineLocal, report.Engine)
	}
	if report.InvocationID == "" {
		t.Error("InvocationID must not be empty")
	}
	// Health must be a valid value.
	switch report.Health {
	case Healthy, Degraded, Unhealthy:
		// ok
	default:
		t.Errorf("unexpected health grade %q", report.Health)
	}
}

func TestRun_GitLabIdentityFromEnv(t *testing.T) {
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_PIPELINE_ID", "12345")
	t.Setenv("CI_JOB_ID", "99")
	t.Setenv("CI_RUNNER_DESCRIPTION", "dungeon-chest-001")
	defer func() {
		os.Unsetenv("GITLAB_CI")
		os.Unsetenv("CI_PIPELINE_ID")
		os.Unsetenv("CI_JOB_ID")
		os.Unsetenv("CI_RUNNER_DESCRIPTION")
	}()

	dir := t.TempDir()
	report := Run(dir, Options{})

	if report.Engine != EngineGitLab {
		t.Errorf("want engine %q, got %q", EngineGitLab, report.Engine)
	}
	if report.InvocationID != "12345" {
		t.Errorf("InvocationID: want %q, got %q", "12345", report.InvocationID)
	}
	if report.Identity.Name != "dungeon-chest-001" {
		t.Errorf("Identity.Name: want %q, got %q", "dungeon-chest-001", report.Identity.Name)
	}
	if report.Identity.JobID != "99" {
		t.Errorf("Identity.JobID: want %q, got %q", "99", report.Identity.JobID)
	}
}
