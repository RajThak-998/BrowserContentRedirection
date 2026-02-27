package main
import "encoding/json"

type Packet struct {
	Type string `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Meta json.RawMessage `json:"meta,omitempty"`
}