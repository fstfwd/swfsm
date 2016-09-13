// Code generated by protoc-gen-go.
// source: test.proto
// DO NOT EDIT!

/*
Package jsonpbserializer is a generated protocol buffer package.

It is generated from these files:
	test.proto

It has these top-level messages:
	TestMessage
*/
package jsonpbserializer

import proto "github.com/golang/protobuf/proto"
import fmt "fmt"
import math "math"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.ProtoPackageIsVersion2 // please upgrade the proto package

type TestMessage struct {
	Item1 string `protobuf:"bytes,1,opt,name=Item1,json=item1" json:"Item1,omitempty"`
	Item2 string `protobuf:"bytes,2,opt,name=Item2,json=item2" json:"Item2,omitempty"`
}

func (m *TestMessage) Reset()                    { *m = TestMessage{} }
func (m *TestMessage) String() string            { return proto.CompactTextString(m) }
func (*TestMessage) ProtoMessage()               {}
func (*TestMessage) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{0} }

func init() {
	proto.RegisterType((*TestMessage)(nil), "jsonpbserializer.TestMessage")
}

func init() { proto.RegisterFile("test.proto", fileDescriptor0) }

var fileDescriptor0 = []byte{
	// 101 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x09, 0x6e, 0x88, 0x02, 0xff, 0xe2, 0xe2, 0x2a, 0x49, 0x2d, 0x2e,
	0xd1, 0x2b, 0x28, 0xca, 0x2f, 0xc9, 0x17, 0x12, 0xc8, 0x2a, 0xce, 0xcf, 0x2b, 0x48, 0x2a, 0x4e,
	0x2d, 0xca, 0x4c, 0xcc, 0xc9, 0xac, 0x4a, 0x2d, 0x52, 0xb2, 0xe4, 0xe2, 0x0e, 0x01, 0xca, 0xfb,
	0xa6, 0x16, 0x17, 0x27, 0xa6, 0xa7, 0x0a, 0x89, 0x70, 0xb1, 0x7a, 0x96, 0xa4, 0xe6, 0x1a, 0x4a,
	0x30, 0x2a, 0x30, 0x6a, 0x70, 0x06, 0xb1, 0x66, 0x82, 0x38, 0x30, 0x51, 0x23, 0x09, 0x26, 0x84,
	0xa8, 0x51, 0x12, 0x1b, 0xd8, 0x4c, 0x63, 0x40, 0x00, 0x00, 0x00, 0xff, 0xff, 0xcb, 0x57, 0x8c,
	0x30, 0x61, 0x00, 0x00, 0x00,
}