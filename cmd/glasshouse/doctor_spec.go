package main

import (
	"glasshouse/internal/backend"
	"glasshouse/internal/policy"
)

func backendSpecForDoctor(arch string, p policy.Policy) backend.Spec {
	return backend.Spec{Arch: arch, CPUs: p.VM.CPUs, MemoryMiB: int(p.VM.Memory >> 20), AllowTCG: false}
}
