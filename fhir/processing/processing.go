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

// Package processing provides utilities for building pipelines to process and
// store FHIR resources.
package processing

import (
	"context"

	"github.com/google/fhir/go/fhirversion"
	"github.com/google/fhir/go/jsonformat"

	cpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/codes_go_proto"
	rpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/bundle_and_contained_resource_go_proto"
)

// ResourceWrapper encapsulates resources to be processed and stored.
type ResourceWrapper interface {
	// Type returns the type of the resource, for easy filtering by processors.
	Type() cpb.ResourceTypeCode_Value
	// SourceURL returns the URL the resource was obtained from.
	SourceURL() string
	// Proto returns a proto which can be mutated by processors.
	Proto() (*rpb.ContainedResource, error)
	// JSON serialises the ContainedResource proto to FHIR JSON.
	JSON() ([]byte, error)
}

type resourceWrapper struct {
	unmarshaller *jsonformat.Unmarshaller
	marshaller   *jsonformat.Marshaller
	resourceType cpb.ResourceTypeCode_Value
	sourceURL    string
	proto        *rpb.ContainedResource
	json         []byte
	// By default, the json field is cleared when the proto is accessed, on the
	// assumption that the proto will be mutated, and thus the JSON would get out
	// of sync. Once processing is done, this flag may be switched to true so that
	// sinks may access both the JSON and the proto at the same time.
	doneMutating bool
}

func (rw *resourceWrapper) Type() cpb.ResourceTypeCode_Value {
	return rw.resourceType
}

func (rw *resourceWrapper) SourceURL() string {
	return rw.sourceURL
}

func (rw *resourceWrapper) Proto() (*rpb.ContainedResource, error) {
	if rw.proto == nil {
		proto, err := rw.unmarshaller.UnmarshalR4(rw.json)
		if err != nil {
			return nil, err
		}
		rw.proto = proto
	}
	if !rw.doneMutating {
		// Clear the json so that it is not out of sync if the proto is mutated.
		rw.json = nil
	}
	return rw.proto, nil
}

func (rw *resourceWrapper) JSON() ([]byte, error) {
	if rw.json == nil {
		json, err := rw.marshaller.Marshal(rw.proto)
		if err != nil {
			return nil, err
		}
		rw.json = json
	}
	return rw.json, nil
}

// Verify resourceWrapper satisfies the ResourceWrapper interface.
var _ ResourceWrapper = &resourceWrapper{}

// OutputFunction is the signature of both Processor.Process and Sink.Write.
type OutputFunction func(ctx context.Context, resource ResourceWrapper) error

// Processor defines a pipeline stage which may mutate resources before they are
// written.
//
// Processors are assumed to not be thread-safe (i.e. it is unsafe to call
// Process from multiple goroutines). Because processors may be chained in a
// Pipeline, Processor implementations must call the sink function set with
// SetSink from exactly on goroutine - either the one from which Process is
// called, or a single goroutine created when the processor is created.
//
// If a processor does create new goroutines, Finalize must not return until all
// of those goroutines have terminated, and the sink function will not be called
// again.
type Processor interface {
	// SetOutput sets where resources should be passed to after processing.
	SetOutput(output OutputFunction)
	// Process a resource as required. This should return an error if SetSink has
	// not yet been called.
	Process(ctx context.Context, resource ResourceWrapper) error
	// Finalize performs any final processing and cleanup. This is called after
	// all resources have been passed to Process(), and so may be used to flush
	// any buffered or batched resources.
	Finalize(ctx context.Context) error
}

// BaseProcessor may be embedded into processor implementations to provide a
// no-op Finalize function and an implementation of SetSink. Structs which embed
// BaseProcessor may call .sink(...) to pass on processed resources.
type BaseProcessor struct {
	Output OutputFunction
}

// SetOutput is Processor.SetOutput. This implementation saves the provided
// output function so that it can be called by a Process function.
func (brp *BaseProcessor) SetOutput(output OutputFunction) {
	brp.Output = output
}

// Finalize is Processor.Finalize. This implementation is a no-op.
func (brp *BaseProcessor) Finalize(ctx context.Context) error {
	return nil
}

// Sink represents a terminal pipeline stage which writes resources to storage.
//
// Sinks are assumed to not be thread-safe (i.e. it is unsafe to call Write from
// multiple goroutines). Sinks may use parallelism and create goroutines
// internally; if so, Finalize must not return until all of those goroutines
// have terminated, and all resources have been written.
type Sink interface {
	// Write a resource to storage.
	Write(ctx context.Context, resource ResourceWrapper) error
	// Perform any final writing and cleanup. This is called after all resources
	// have been passed to Write(), and so may be used to flush any buffered or
	// batched resources.
	Finalize(ctx context.Context) error
}

// A Pipeline consumes FHIR resources (as JSON), applies processing steps, and
// then writes the resources to zero or more sinks.
type Pipeline struct {
	unmarshaller *jsonformat.Unmarshaller
	marshaller   *jsonformat.Marshaller
	processors   []Processor
	sinks        []Sink
	pipelineFunc OutputFunction
}

// NewPipeline constructs a new Pipeline, plumbing together the given Processors
// and Sinks. Both processors and sinks may be empty if no processing or output
// is required. Note that processors and sinks should not be shared between
// pipelines.
func NewPipeline(processors []Processor, sinks []Sink) (*Pipeline, error) {
	unmarshaller, err := jsonformat.NewUnmarshallerWithoutValidation("UTC", fhirversion.R4)
	if err != nil {
		return nil, err
	}
	marshaller, err := jsonformat.NewMarshaller(false, "", "", fhirversion.R4)
	if err != nil {
		return nil, err
	}
	p := &Pipeline{
		unmarshaller: unmarshaller,
		marshaller:   marshaller,
		processors:   processors,
		sinks:        sinks,
	}
	// Build the pipeline function by applying each processing step on top of the
	// sinks, starting from the last so that the processing steps are applied in
	// the same order they are passed to this function. If there are no
	// processors, the pipeline function is just writing to the sinks (and if
	// there are also no sinks the pipeline is a no-op).
	p.pipelineFunc = p.writeToSinks
	for i := len(processors) - 1; i >= 0; i-- {
		processors[i].SetOutput(p.pipelineFunc)
		p.pipelineFunc = processors[i].Process
	}
	return p, nil
}

// writeToSinks writes the resource to each sink sequentially.
func (p *Pipeline) writeToSinks(ctx context.Context, resource ResourceWrapper) error {
	if rw, ok := resource.(*resourceWrapper); ok {
		rw.doneMutating = true
	}
	for _, s := range p.sinks {
		if err := s.Write(ctx, resource); err != nil {
			return err
		}
	}
	return nil
}

// Process a single FHIR resource. The resource is passed through the processing
// steps to the sinks.
//
// Pipelines do not apply any parallel processing. Resources pass through each
// processing step sequentially, and are written to each sink sequentially; this
// function returns only when the processor and sinks return. If a processor or
// sink needs to perform heavy lifting, it may use parallelism internally.
//
// It is not safe to call this function from multiple Goroutines.
func (p *Pipeline) Process(ctx context.Context, resourceType cpb.ResourceTypeCode_Value, sourceURL string, json []byte) error {
	return p.pipelineFunc(ctx, &resourceWrapper{
		unmarshaller: p.unmarshaller,
		marshaller:   p.marshaller,
		resourceType: resourceType,
		sourceURL:    sourceURL,
		json:         json,
	})
}

// Finalize calls finalize on all of the underlying Processors and Sinks in the
// pipeline, returning the first error seen.
func (p *Pipeline) Finalize(ctx context.Context) error {
	for _, pr := range p.processors {
		if err := pr.Finalize(ctx); err != nil {
			return err
		}
	}
	for _, s := range p.sinks {
		if err := s.Finalize(ctx); err != nil {
			return err
		}
	}
	return nil
}