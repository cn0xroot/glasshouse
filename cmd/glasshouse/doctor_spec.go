package main

import (
	"glasshouse/internal/backend"
	"glasshouse/internal/policy"
	"glasshouse/internal/session"
)

func backendSpecForDoctor(home, arch string, p policy.Policy) backend.Spec {
	return backend.Spec{Dir: session.Dir(home, "s-000000"), Arch: arch, CPUs: p.VM.CPUs, MemoryMiB: int(p.VM.Memory >> 20), AllowTCG: false}
}
