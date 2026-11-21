package meshapi

// Modality (v2.13) is what kind of model a capacity, status or fleet entry
// describes: what a client sends it and what comes back.
//
// Absent means text. That reading is accurate rather than hopeful: every node
// before this version serves only text models, and a viiwork node's own
// models are always text, so it never writes the field. A program that joins
// the mesh to serve something else (viiwork-voice: full-duplex audio
// sessions, later image generation) publishes the modality on each model.
//
// A reader must treat an unknown non-empty value as not text: such a model is
// never a route for text inference (chat, completions, embeddings or any
// other client dialect), whatever else the reader makes of it. A new modality
// is then additive in the same way a new field is.
const (
	ModalityText  = "text"
	ModalityVoice = "voice"
	ModalityImage = "image"
)

// IsTextModality reports whether a published modality is text inference:
// true for absent ("") and "text", false for anything else, known or not.
func IsTextModality(s string) bool { return s == "" || s == ModalityText }

// ErrCodeModelModality is ErrorBody.Code on the 400 a node answers when a
// text inference request names a model the fleet serves only with another
// modality.
const ErrCodeModelModality = "model_modality"
