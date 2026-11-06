/*
Copyright 2026 The CloudPilot AI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package environment

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestExistingNamespaceNeedsNoCreatePermission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/"+TestNamespace {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonForbidden, Code: 403})
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.Namespace{TypeMeta: metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: TestNamespace, UID: "preserved-bootstrap", Labels: map[string]string{"karpenter-e2e/owned": "true"}}})
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := &Environment{KubeClient: client}
	if err := env.ensureTestNamespace(); err != nil {
		t.Fatalf("existing namespace must work with GET-only authorization: %v", err)
	}
}

func TestNamespaceLocalCreationAndReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		getStatus, createStatus int
		wantError               bool
	}{
		{"missing_local_creates", 404, 201, false},
		{"concurrent_local_creation", 404, 409, false},
		{"missing_ci_cannot_repair", 404, 403, true},
		{"forbidden_read_is_not_creation_authority", 403, 201, true},
		{"unavailable_read_is_not_creation_authority", 503, 201, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				status := tc.getStatus
				if r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces" {
					var created corev1.Namespace
					if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
						t.Error(err)
					}
					if created.Labels["karpenter-e2e/owned"] != "true" {
						t.Errorf("local namespace is not recognized by owned cleanup: %+v", created.ObjectMeta)
					}
					status = tc.createStatus
				} else if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/"+TestNamespace {
					t.Errorf("unexpected API %s %s", r.Method, r.URL.Path)
					status = 500
				}
				w.WriteHeader(status)
				if status == 201 {
					_ = json.NewEncoder(w).Encode(corev1.Namespace{TypeMeta: metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: TestNamespace}})
					return
				}
				reason := map[int]metav1.StatusReason{404: metav1.StatusReasonNotFound, 403: metav1.StatusReasonForbidden, 409: metav1.StatusReasonAlreadyExists, 503: metav1.StatusReasonServiceUnavailable}[status]
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: reason, Code: int32(status)})
			}))
			defer server.Close()
			client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
			if err != nil {
				t.Fatal(err)
			}
			err = (&Environment{KubeClient: client}).ensureTestNamespace()
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%v", err, tc.wantError)
			}
		})
	}
}
