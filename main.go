package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v2"
)

// applyYAML is a utility function to apply YAML using kubectl
func applyYAML(yamlContent interface{}, filename string) error {
	data, err := yaml.Marshal(yamlContent)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}

	err = ioutil.WriteFile(filename, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write YAML to file: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", filename)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute kubectl apply: %w", err)
	}
	return nil
}

// Health check
func hello(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Crossplane OpenStack API is running.")
}

// Register team (namespace)
func registerTeam(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, fmt.Sprintf("Error decoding request: %v", err), http.StatusBadRequest)
		return
	}

	if data.Name == "" {
		http.Error(w, "Missing 'name' in request", http.StatusBadRequest)
		return
	}

	namespaceYAML := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]string{
			"name": data.Name,
		},
	}

	filename := filepath.Join(os.TempDir(), fmt.Sprintf("namespace-%s.yaml", data.Name))
	defer os.Remove(filename) // Clean up temp file

	if err := applyYAML(namespaceYAML, filename); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create namespace: %v", err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("Namespace '%s' created successfully.", data.Name)})
}

// Get team details
func getTeam(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamID := vars["team_id"]

	cmd := exec.Command("kubectl", "get", "namespace", teamID, "-o", "json")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to get namespace details: %v\n%s", err, stderr.String()), http.StatusInternalServerError)
		return
	}

	var result interface{}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse kubectl output: %v", err), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(result)
}

// Create VM
func createVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamID := vars["team_id"]

	var data struct {
		Name          string   `json:"name"`
		ImageID       string   `json:"imageId"`
		FlavorID      string   `json:"flavorId"`
		NetworkID     string   `json:"networkId"`
		SecurityGroups []string `json:"securityGroups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, fmt.Sprintf("Error decoding request: %v", err), http.StatusBadRequest)
		return
	}

	if data.Name == "" || data.ImageID == "" || data.FlavorID == "" || data.NetworkID == "" {
		http.Error(w, "Missing required fields", http.StatusBadRequest)
		return
	}
	if len(data.SecurityGroups) == 0 {
		data.SecurityGroups = []string{"default"}
	}

	instanceYAML := map[string]interface{}{
		"apiVersion": "compute.openstack.crossplane.io/v1alpha1",
		"kind":       "InstanceV2",
		"metadata": map[string]string{
			"name":      data.Name,
			"namespace": teamID,
		},
		"spec": map[string]interface{}{
			"forProvider": map[string]interface{}{
				"configDrive":    true,
				"flavorId":       data.FlavorID,
				"imageId":        data.ImageID,
				"name":           data.Name,
				"network":        []map[string]string{{"uuid": data.NetworkID}},
				"securityGroups": data.SecurityGroups,
			},
			"providerConfigRef": map[string]string{
				"name": "provider-openstack-config",
			},
		},
	}

	yamlPath := filepath.Join("/home/ubuntu/crossplane-api", fmt.Sprintf("%s-%s.yaml", teamID, data.Name))
	if err := applyYAML(instanceYAML, yamlPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to provision VM: %v", err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("VM '%s' provisioned successfully in namespace '%s'.", data.Name, teamID)})
}

// Resize VM
func resizeVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamName := vars["team_name"]
	vmName := vars["vm_name"]

	var data struct {
		FlavorID string `json:"flavorId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, fmt.Sprintf("Error decoding request: %v", err), http.StatusBadRequest)
		return
	}

	if data.FlavorID == "" {
		http.Error(w, "Missing 'flavorId' in request", http.StatusBadRequest)
		return
	}

	yamlPath := filepath.Join("/home/ubuntu/crossplane-api", fmt.Sprintf("%s-%s.yaml", teamName, vmName))
	if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("%s not found", yamlPath), http.StatusNotFound)
		return
	}

	fileContent, err := ioutil.ReadFile(yamlPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read VM YAML: %v", err), http.StatusInternalServerError)
		return
	}

	var instance map[string]interface{}
	if err := yaml.Unmarshal(fileContent, &instance); err != nil {
		http.Error(w, fmt.Sprintf("Failed to unmarshal VM YAML: %v", err), http.StatusInternalServerError)
		return
	}

	if spec, ok := instance["spec"].(map[interface{}]interface{}); ok {
		if forProvider, ok := spec["forProvider"].(map[interface{}]interface{}); ok {
			forProvider["flavorId"] = data.FlavorID
		}
	}

	if err := applyYAML(instance, yamlPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to update VM flavor: %v", err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("Flavor for VM '%s' updated successfully.", vmName)})
}

// Scale VM (Note: This assumes a Kubernetes Deployment/StatefulSet for scaling, not directly a Crossplane InstanceV2 resource)
func scaleVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamID := vars["team_id"]
	resourceID := vars["resource_id"]

	var data struct {
		Replicas *int `json:"replicas"` // Use pointer to distinguish between missing and 0
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, fmt.Sprintf("Error decoding request: %v", err), http.StatusBadRequest)
		return
	}

	if data.Replicas == nil {
		http.Error(w, "Missing 'replicas' in request", http.StatusBadRequest)
		return
	}

	cmd := exec.Command("kubectl", "scale", fmt.Sprintf("deployment/%s", resourceID), fmt.Sprintf("--replicas=%d", *data.Replicas), "-n", teamID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to scale VM: %v\n%s", err, stderr.String()), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("Scaled VM '%s' to %d replicas.", resourceID, *data.Replicas)})
}

// Attach Disk
func attachDisk(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamName := vars["team_name"]
	vmName := vars["vm_name"] // This vm_name is used for naming the attachment, not necessarily the actual instance ID.

	var data struct {
		VolumeID   string `json:"volumeId"`
		InstanceID string `json:"instanceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, fmt.Sprintf("Error decoding request: %v", err), http.StatusBadRequest)
		return
	}

	if data.VolumeID == "" || data.InstanceID == "" {
		http.Error(w, "Missing volumeId or instanceId", http.StatusBadRequest)
		return
	}

	// In Go, we don't have uuid.uuid4() directly from standard library for simple strings.
	// You can use a dedicated UUID package or generate a random string.
	// For simplicity, let's use a combination of current time and some random bytes.
	// For production, consider a proper UUID library like github.com/google/uuid
	nameSuffix := fmt.Sprintf("%x", os.Getpid())
	attachmentName := fmt.Sprintf("%s-attach-%s", vmName, nameSuffix[:8]) // Truncate for brevity

	volumeAttachment := map[string]interface{}{
		"apiVersion": "compute.openstack.crossplane.io/v1alpha1",
		"kind":       "VolumeAttachmentV2",
		"metadata": map[string]string{
			"name":      attachmentName,
			"namespace": teamName,
		},
		"spec": map[string]interface{}{
			"instanceId": data.InstanceID,
			"volumeId":   data.VolumeID,
			"providerConfigRef": map[string]string{
				"name": "provider-openstack-config",
			},
			"deletionPolicy": "Delete",
		},
	}

	yamlPath := filepath.Join(os.TempDir(), fmt.Sprintf("%s.yaml", attachmentName))
	defer os.Remove(yamlPath)

	if err := applyYAML(volumeAttachment, yamlPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to attach disk: %v", err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("Disk attachment request sent: %s", attachmentName)})
}

// Create Block Volume
func createBlockVolume(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamName := vars["team_name"]

	var data struct {
		Name        string `json:"name"`
		Size        int    `json:"size"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, fmt.Sprintf("Error decoding request: %v", err), http.StatusBadRequest)
		return
	}

	if data.Name == "" || data.Size == 0 {
		http.Error(w, "Missing required fields (name or size)", http.StatusBadRequest)
		return
	}

	volumeManifest := map[string]interface{}{
		"apiVersion": "blockstorage.openstack.crossplane.io/v1alpha1",
		"kind":       "VolumeV3",
		"metadata": map[string]string{
			"name":      data.Name,
			"namespace": teamName,
		},
		"spec": map[string]interface{}{
			"forProvider": map[string]interface{}{
				"name":        data.Name,
				"size":        data.Size,
				"description": data.Description,
			},
			"providerConfigRef": map[string]string{
				"name": "provider-openstack-config",
			},
		},
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s-block.yaml", data.Name))
	defer os.Remove(tmpFile)

	fileContent, err := yaml.Marshal(volumeManifest)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to marshal volume manifest: %v", err), http.StatusInternalServerError)
		return
	}
	if err := ioutil.WriteFile(tmpFile, fileContent, 0644); err != nil {
		http.Error(w, fmt.Sprintf("Failed to write volume manifest to file: %v", err), http.StatusInternalServerError)
		return
	}

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to apply block volume manifest: %v\nDetails: %s", err, stderr.String()), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"message":        fmt.Sprintf("Block volume '%s' created successfully in namespace '%s'.", data.Name, teamName),
		"kubectl_output": stdout.String(),
	})
}

// Start/Stop/Delete VM actions
func handleVMAction(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamName := vars["team_name"]
	vmName := vars["vm_name"]
	action := vars["action"]
	script := "/home/ubuntu/admin.sh"

	// ---- Check VM task_state ----
	var statusOut, statusErr bytes.Buffer
	statusCmd := exec.Command("bash", "-c", fmt.Sprintf("source %s && openstack server show %s -f json", script, vmName))
	statusCmd.Stdout = &statusOut
	statusCmd.Stderr = &statusErr

	if err := statusCmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch VM status: %v\n%s", err, statusErr.String()), http.StatusInternalServerError)
		return
	}

	var statusData map[string]interface{}
	if err := json.Unmarshal(statusOut.Bytes(), &statusData); err != nil {
		http.Error(w, "Failed to parse VM status", http.StatusInternalServerError)
		return
	}

	if taskState, ok := statusData["OS-EXT-STS:task_state"].(string); ok && taskState != "" {
		http.Error(w, fmt.Sprintf("VM is currently busy (task_state: %s). Try again later.", taskState), http.StatusConflict)
		return
	}

	// ---- Construct the command ----
	var cmd *exec.Cmd
	switch action {
	case "start":
		cmd = exec.Command("bash", "-c", fmt.Sprintf("source %s && openstack server start %s", script, vmName))
	case "stop":
		cmd = exec.Command("bash", "-c", fmt.Sprintf("source %s && openstack server stop %s", script, vmName))
	case "delete":
		cmd = exec.Command("bash", "-c", fmt.Sprintf("source %s && kubectl delete instancev2 %s --namespace %s", script, vmName, teamName))
	default:
		http.Error(w, fmt.Sprintf("Unsupported action '%s'.", action), http.StatusBadRequest)
		return
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// ---- Run action command ----
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to execute action '%s' on VM '%s': %v\n%s", action, vmName, err, stderr.String()), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Action '%s' executed on VM '%s'. Output:\n%s", action, vmName, stdout.String()),
	})
}



// Delete VM directly
func deleteVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	teamID := vars["team_id"]
	resourceID := vars["resource_id"]

	cmd := exec.Command("kubectl", "delete", "instancev2", resourceID, "-n", teamID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete VM: %v\n%s", err, stderr.String()), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("VM '%s' deleted from team '%s'.", resourceID, teamID)})
}

func main() {
	router := mux.NewRouter()

	router.HandleFunc("/", hello).Methods("GET")
	router.HandleFunc("/teams", registerTeam).Methods("POST")
	router.HandleFunc("/teams/{team_id}", getTeam).Methods("GET")
	router.HandleFunc("/teams/{team_id}/vm", createVM).Methods("POST")
	router.HandleFunc("/teams/{team_name}/vm/{vm_name}/resize", resizeVM).Methods("PUT")
	router.HandleFunc("/teams/{team_id}/vm/{resource_id}/scale", scaleVM).Methods("PUT")
	router.HandleFunc("/teams/{team_name}/vm/{vm_name}/attach-disk", attachDisk).Methods("POST")
	router.HandleFunc("/teams/{team_name}/block", createBlockVolume).Methods("POST")
	router.HandleFunc("/teams/{team_name}/vm/{vm_name}/{action}", handleVMAction).Methods("PUT") // Combined start/stop/delete via action
	router.HandleFunc("/teams/{team_id}/vm/{resource_id}", deleteVM).Methods("DELETE")

	port := "8080"
	log.Printf("Server starting on port %s...", port)
	log.Fatal(http.ListenAndServe(":"+port, router))
}
