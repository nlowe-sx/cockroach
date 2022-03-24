// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Package embeddedproj defines the format used to embed static projection data
// in the Cockroach binary. Is is used to load the data as well as to generate
// the data file (from cmd/generate-spatial-ref-sys).
package embeddedproj

import (
	"compress/gzip"
	"encoding/json"
	"io"
)

// Spheroid stores the metadata for a spheroid. Each spheroid is referenced by
// its unique hash.
type Spheroid struct {
	Hash       int64
	Radius     float64
	Flattening float64
}

// Projection stores the metadata for a projection; it mirrors the fields in
// geoprojbase.ProjInfo but with modifications that allow serialization and
// deserialization.
type Projection struct {
	SRID      int
	AuthName  string
	AuthSRID  int
	SRText    string
	Proj4Text string
	Bounds    Bounds
	IsLatLng  bool
	// The hash of the spheroid represented by the SRID.
	Spheroid int64
}

// Bounds stores the bounds of a projection.
type Bounds struct {
	MinX float64
	MaxX float64
	MinY float64
	MaxY float64
}

// Data stores all the spheroid and projection data.
type Data struct {
	Spheroids   []Spheroid
	Projections []Projection
}

// Encode writes serializes Data as a gzip-compressed json.
func Encode(d Data, w io.Writer) error {
	data, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(data); err != nil {
		return err
	}
	return zw.Close()
}

// Decode deserializes Data from a gzip-compressed json generated by Encode().
func Decode(r io.Reader) (Data, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Data{}, err
	}
	var result Data
	if err := json.NewDecoder(zr).Decode(&result); err != nil {
		return Data{}, err
	}
	return result, nil
}