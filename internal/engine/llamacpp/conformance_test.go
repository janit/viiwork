package llamacpp

import (
	"testing"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/engine/enginetest"
	"github.com/janit/viiwork/v2/internal/gpu"
)

// llama.cpp is the reference implementation of C7, so it is also the kit's
// first subject: an assertion it cannot satisfy is a finding about the kit or
// about the contract, not a reason to weaken either.
func TestConformance(t *testing.T) {
	enginetest.Run(t, New(),
		enginetest.Case{
			Name: "single card",
			Spec: engine.Spec{
				Name: "conformance-model", Path: "/models/conformance.gguf",
				GPUs: []int{0}, Port: 41999, Vendor: gpu.VendorAMD,
				Context: 8192, Parallel: 4, Backends: 1,
			},
			Options: "binary: llama-server\nthreads: 4\n",
		},
		enginetest.Case{
			Name: "split pair",
			Spec: engine.Spec{
				Name: "conformance-split", Path: "/models/conformance.gguf",
				GPUs: []int{0, 1}, Port: 41998, Vendor: gpu.VendorAMD,
				Context: 49152, Parallel: 2, Backends: 3,
			},
			Options: "split_mode: layer\nsplit_weights: [1.16, 1.0]\n",
		},
		enginetest.Case{
			Name: "cpu backend",
			Spec: engine.Spec{
				Name: "conformance-cpu", Path: "/models/conformance.gguf",
				Port: 41997, Vendor: gpu.VendorNone, Context: 4096, Parallel: 1, Backends: 1,
			},
		},
	)
}
