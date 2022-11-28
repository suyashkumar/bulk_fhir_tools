// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package iohelpers contains helper structs and libraries to assist with io.
// This is an internal package.
package iohelpers

import (
	"errors"
	"io"
	"strings"
)

// MultiWriteCloser wraps multiple WriteClosers into a single one.
type MultiWriteCloser struct {
	writerClosers []io.WriteCloser
}

// Add a new WriteCloser to the multiWriteCloser.
func (m *MultiWriteCloser) Add(wc io.WriteCloser) {
	m.writerClosers = append(m.writerClosers, wc)
}

// Write writes the specified bytes to every WriteCloser within this
// multiWriteCloser
func (m *MultiWriteCloser) Write(p []byte) (n int, err error) {
	for _, w := range m.writerClosers {
		n, err = w.Write(p)
		if err != nil {
			return n, err
		}
		if n != len(p) {
			return n, io.ErrShortWrite
		}
	}
	return len(p), nil
}

// Close closes every WriteCloser within this MultiWriteCloser. If multiple
// have errors on close, the errors are combined into a single error and
// returned.
func (m *MultiWriteCloser) Close() error {
	errStrings := make([]string, 0, len(m.writerClosers))
	for _, w := range m.writerClosers {
		if err := w.Close(); err != nil {
			errStrings = append(errStrings, err.Error())
		}
	}

	if len(errStrings) != 0 {
		return errors.New(strings.Join(errStrings, ","))
	}

	return nil
}
