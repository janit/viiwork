package energy

import (
	"encoding/binary"
	"math"
)

// Record sizes are fixed and part of the on-disk format. They are the reason
// the whole 365-day history fits in single-digit megabytes: see Store for the
// budget.
const (
	NodeRecordSize = 16
	GPURecordSize  = 24
)

// NodeRecord is one time bucket of whole-node power, as measured over IPMI.
type NodeRecord struct {
	// TS is the bucket start in unix seconds. Zero means the slot was never
	// written; a real bucket never lands on the epoch.
	TS int64
	// Watts is the mean over the *covered* part of the bucket, not over its
	// whole span.
	Watts float32
	// CoveredS is how many seconds of the bucket were actually sampled. It is
	// what keeps a restart honest: a bucket that saw 20s of samples contributes
	// 20s of energy rather than a full bucket extrapolated from a fragment.
	CoveredS uint16
}

// KWh is the energy this bucket represents, counting only covered time.
func (r NodeRecord) KWh() float64 {
	return float64(r.Watts) * float64(r.CoveredS) / 3600 / 1000
}

func (r NodeRecord) encode(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], uint64(r.TS))
	binary.LittleEndian.PutUint32(b[8:], math.Float32bits(r.Watts))
	binary.LittleEndian.PutUint16(b[12:], r.CoveredS)
	binary.LittleEndian.PutUint16(b[14:], 0)
}

func decodeNode(b []byte) NodeRecord {
	return NodeRecord{
		TS:       int64(binary.LittleEndian.Uint64(b[0:])),
		Watts:    math.Float32frombits(binary.LittleEndian.Uint32(b[8:])),
		CoveredS: binary.LittleEndian.Uint16(b[12:]),
	}
}

// GPURecord is one time bucket for one GPU.
type GPURecord struct {
	TS    int64
	GPUID uint16
	// ModelIdx indexes the node-local model table. Carrying it per record is
	// what makes a per-model rollup exact across a reconfiguration: the model
	// that was resident during the bucket is recorded with the bucket, so there
	// is no occupancy log to join against and no way for the two to disagree.
	ModelIdx uint16
	// AttrW is this GPU's share of measured node power (see Attribute), which
	// is the number to sum for "what did this model cost".
	AttrW float32
	// RawW is the unreconciled rocm-smi reading, kept so a calibration argument
	// can be settled later from recorded data rather than from a new experiment.
	RawW     float32
	CoveredS uint16
}

// KWh is the attributed energy for this bucket, counting only covered time.
func (r GPURecord) KWh() float64 {
	return float64(r.AttrW) * float64(r.CoveredS) / 3600 / 1000
}

func (r GPURecord) encode(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], uint64(r.TS))
	binary.LittleEndian.PutUint16(b[8:], r.GPUID)
	binary.LittleEndian.PutUint16(b[10:], r.ModelIdx)
	binary.LittleEndian.PutUint32(b[12:], math.Float32bits(r.AttrW))
	binary.LittleEndian.PutUint32(b[16:], math.Float32bits(r.RawW))
	binary.LittleEndian.PutUint16(b[20:], r.CoveredS)
	binary.LittleEndian.PutUint16(b[22:], 0)
}

func decodeGPU(b []byte) GPURecord {
	return GPURecord{
		TS:       int64(binary.LittleEndian.Uint64(b[0:])),
		GPUID:    binary.LittleEndian.Uint16(b[8:]),
		ModelIdx: binary.LittleEndian.Uint16(b[10:]),
		AttrW:    math.Float32frombits(binary.LittleEndian.Uint32(b[12:])),
		RawW:     math.Float32frombits(binary.LittleEndian.Uint32(b[16:])),
		CoveredS: binary.LittleEndian.Uint16(b[20:]),
	}
}
