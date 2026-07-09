package analysis_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func FuzzTaskSerialization(f *testing.F) {
	f.Add(`{"key":"value"}`)
	f.Add(`{}`)
	f.Add(`{"nested":{"a":1}}`)
	f.Add("")

	f.Fuzz(func(t *testing.T, input string) {
		payload, err := structpb.NewStruct(map[string]interface{}{"data": input})
		if err != nil {
			return
		}
		data, err := proto.Marshal(payload)
		if err != nil {
			return
		}
		var decoded structpb.Struct
		_ = proto.Unmarshal(data, &decoded)
	})
}

func FuzzResultDeserialization(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Add([]byte("not proto"))

	f.Fuzz(func(t *testing.T, data []byte) {
		var s structpb.Struct
		_ = proto.Unmarshal(data, &s)
	})
}
