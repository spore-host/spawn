package sweep

// CrossAccountRoleName is the role the sweep orchestrator assumes in the
// account a detached sweep's members launch in. Every member is launched,
// tagged and cancelled through it (lambda/sweep-orchestrator). spawn creates
// and converges it (pkg/aws EnsureSweepCrossAccountRole, #1537) and removes it
// with `spawn sweep teardown`.
const CrossAccountRoleName = "SpawnSweepCrossAccountRole"
