package provenance

import (
	"testing"
	"time"
)

// #1231: a run whose container had the host Docker socket started containers
// the recipe does not record, so the sweeper can't prove a rerun would use the
// same ones: it must never be provable. The same run without the socket is.
func TestReportDoesNotProveARunThatHadTheDockerSocket(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	sock := rec("socket", true)
	sock.Recipe.DockerSocket = true
	b := &fakeBucket{now: now, objs: []fakeObj{
		{key: "out/socket/a", bytes: 10, tags: outTags("socket", ClassCheap, true), record: sock},
		{key: "out/plain/a", bytes: 10, tags: outTags("plain", ClassCheap, true), record: rec("plain", true)},
	}}
	rep := build(t, b, &fakeInputs{})
	if so := runByID(t, rep, "socket"); so.Provable || !whyHas(so, "Docker socket") {
		t.Errorf("a run that had the Docker socket can't be proven: provable=%v %v", so.Provable, so.WhyNot)
	}
	if pl := runByID(t, rep, "plain"); !pl.Provable {
		t.Errorf("the same run without the socket must be provable: %v", pl.WhyNot)
	}
}
