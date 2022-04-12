// Package testhelpers provides common testing helpers and utilities that are used
// across packages in this project. Only non-trivial helper logic used in
// multiple places in this library should be added here.
package testhelpers

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// FHIRStoreTestResource represents a test FHIR resource to be uploaded to
// FHIR store.
type FHIRStoreTestResource struct {
	ResourceID   string
	ResourceType string
	Data         []byte
}

// FHIRStoreServer creates a test FHIR store server that expects the provided
// expectedResources. If it receives valid upload requests that do not include
// elements from expectedResources, it will call t.Errorf with an error. If not
// all of the resources in expectedResources are uploaded by the end of the test
// errors are thrown. The test server's URL is returned by this function, and
// is auto-closed at the end of the test.
func FHIRStoreServer(t *testing.T, expectedResources []FHIRStoreTestResource, projectID, location, datasetID, fhirStoreID string) string {
	t.Helper()
	var expectedResourceWasUploadedMutex sync.Mutex
	expectedResourceWasUploaded := make([]bool, len(expectedResources))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		expectedResource, expectedResourceIdx := validateURLAndMatchResource(req.URL.String(), expectedResources, projectID, location, datasetID, fhirStoreID)
		if expectedResource == nil {
			t.Errorf("FHIR Store Test server received an unexpected request at url: %s", req.URL.String())
			w.WriteHeader(500)
			return
		}
		if req.Method != http.MethodPut {
			t.Errorf("FHIR Store test server unexpected HTTP method. got: %v, want: %v", req.Method, http.MethodPut)
		}

		bodyContent, err := ioutil.ReadAll(req.Body)
		if err != nil {
			t.Errorf("FHIR Store test server error reading body content for URL: %s", req.URL.String())
		}
		if !cmp.Equal(NormalizeJSON(t, bodyContent), NormalizeJSON(t, expectedResource.Data)) {
			t.Errorf("FHIR store test server received unexpected body content. got: %s, want: %s", bodyContent, expectedResource.Data)
		}

		// Update the corresponding index in expectedResourceWasUploaded slice.
		expectedResourceWasUploadedMutex.Lock()
		expectedResourceWasUploaded[expectedResourceIdx] = true
		expectedResourceWasUploadedMutex.Unlock()

		w.WriteHeader(200) // Send OK status code.
	}))

	t.Cleanup(func() {
		server.Close()
		for idx, val := range expectedResourceWasUploaded {
			if !val {
				t.Errorf("FHIR store test server error. Expected resource was not uploaded. got: nil, want: %v", expectedResources[idx])
			}
		}
	})
	return server.URL
}

func validateURLAndMatchResource(callURL string, expectedResources []FHIRStoreTestResource, projectID, location, datasetID, fhirStoreID string) (*FHIRStoreTestResource, int) {
	for idx, r := range expectedResources {
		expectedPath := fmt.Sprintf("/v1/projects/%s/locations/%s/datasets/%s/fhirStores/%s/fhir/%s/%s?", projectID, location, datasetID, fhirStoreID, r.ResourceType, r.ResourceID)
		if callURL == expectedPath {
			return &r, idx
		}
	}
	return nil, 0
}