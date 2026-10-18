/*
Copyright 2021 KubeCube Authors

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

package handler

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/kubecube-io/kubecube/pkg/clog"
	"kubecube-webconsole/utils"
)

// The terminal is a blind byte pipe: the console sees the bytes the pod writes
// and nothing about what they mean. `kubectl get` answers with a table, and a
// table is a projection of the object — `-o wide` is the same summary view with
// more columns, not a different kind of output. An explicit format such as
// `-o json`, `-o yaml` or `-o jsonpath` prints the object itself, or the
// projection the user asked for, so there is nothing left to explain. The
// gateway — the only component on the path that reads the command — says which
// one it was out of band. `toast` is the operation metadata.go documents for
// exactly this, and this is its only producer.
const (
	// hintSummary holds for the command that was just run: the read permission
	// that let `kubectl get <res>` print anything also allows the yaml of the
	// same object, so this line needs no permission check.
	hintSummary = "提示：表格只显示资源摘要，查看完整对象请用 kubectl get %s -o yaml"
	// hintRelated is built only from resources the platform confirmed this user
	// may read, and it spells out a command instead of naming a concept.
	hintRelated = "你还可以查看关联资源：kubectl get %s%s"
)

// authorizationResourcesPath is the platform's batch permission API. The console
// calls it to decide which entries to offer, so the terminal asks the same
// question of the same source rather than inventing a second notion of scope.
const authorizationResourcesPath = "/api/v1/cube/authorization/resources"

// defaultNamespace is what kubectl resolves to when neither the command nor the
// kubeconfig context names a namespace. A CloudShell jail's kubeconfig comes
// from kubeconfig.BuildKubeConfigForUser, which writes no namespace into its
// context, so a bare `kubectl get pods` reads `default`.
const defaultNamespace = "default"

// permissionHTTPClient calls the platform from the terminal's input path, so it
// carries a deadline: a platform that does not answer costs the hint, never a
// keystroke. The platform's certificate is not verified for the same reason
// auth.go does not verify it — the gateway carries no CA for that endpoint.
var permissionHTTPClient = &http.Client{
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	Timeout:   3 * time.Second,
}

// relatedResources names, per listed resource, the resources that usually
// explain what the listed objects are doing. Only a resource in this table gets
// the second line, and that line names only the related resources the platform
// confirms this user may read.
var relatedResources = map[string][]string{
	"pods":                     {"replicasets", "services"},
	"deployments":              {"replicasets", "pods"},
	"statefulsets":             {"pods"},
	"daemonsets":               {"pods"},
	"replicasets":              {"pods"},
	"jobs":                     {"pods"},
	"cronjobs":                 {"jobs", "pods"},
	"services":                 {"pods", "ingresses"},
	"ingresses":                {"services"},
	"horizontalpodautoscalers": {"deployments"},
}

// resourceAliases maps the names kubectl accepts onto the plural RBAC names.
// RBAC rules are written in plurals, so `kubectl get deploy` has to be asked
// about as `deployments`; asked as `deploy` the permission API would answer
// about a resource no rule mentions, and the hint would lose its second line.
var resourceAliases = map[string]string{
	"po": "pods", "pod": "pods", "pods": "pods",
	"deploy": "deployments", "deployment": "deployments", "deployments": "deployments",
	"sts": "statefulsets", "statefulset": "statefulsets", "statefulsets": "statefulsets",
	"ds": "daemonsets", "daemonset": "daemonsets", "daemonsets": "daemonsets",
	"rs": "replicasets", "replicaset": "replicasets", "replicasets": "replicasets",
	"job": "jobs", "jobs": "jobs",
	"cj": "cronjobs", "cronjob": "cronjobs", "cronjobs": "cronjobs",
	"svc": "services", "service": "services", "services": "services",
	"ing": "ingresses", "ingress": "ingresses", "ingresses": "ingresses",
	"hpa": "horizontalpodautoscalers", "horizontalpodautoscaler": "horizontalpodautoscalers",
	"horizontalpodautoscalers": "horizontalpodautoscalers",
}

// getFlagsWithValue are the `kubectl get` flags that consume the next field. A
// flag missing from this set is read as a boolean, which can only misread the
// resource name; the permission API then answers false for a name no rule
// mentions, and the hint loses its second line rather than saying something
// untrue.
var getFlagsWithValue = map[string]bool{
	"-n": true, "--namespace": true,
	"-o": true, "--output": true,
	"-l": true, "--selector": true,
	"--field-selector": true, "--sort-by": true, "--template": true,
	"-f": true, "--filename": true,
	"--context": true, "--kubeconfig": true, "--cluster": true, "--user": true,
	"--as": true, "--as-group": true, "--chunk-size": true, "--request-timeout": true,
}

// getCommand is the part of a `kubectl get` line the hint needs.
type getCommand struct {
	// resource is the name the user typed, so the suggested command is one they
	// can paste back unchanged.
	resource string
	// canonical is the plural RBAC name of resource, or "" for a resource this
	// hint has nothing to say about.
	canonical string
	// namespace is the namespace the command reads; "" means every namespace.
	namespace string
}

// namespaceFlag pins the suggested command to the scope the user's own command
// used, so the suggestion lists what the table just listed.
func (c getCommand) namespaceFlag() string {
	if c.namespace == "" {
		return " -A"
	}
	return " -n " + c.namespace
}

// parseKubectlGet recognises a `kubectl get` line that renders a table. `table`
// and `wide` are that table — `wide` is the same summary view with more columns
// — so both are accepted. An explicit format such as `json`, `yaml` or
// `jsonpath` prints the object itself or the projection the user asked for, and
// the hint explains what a table hides, which is noise on a command that
// already printed the object.
func parseKubectlGet(line string) (getCommand, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || path.Base(fields[0]) != "kubectl" || fields[1] != "get" {
		return getCommand{}, false
	}

	cmd := getCommand{namespace: defaultNamespace}
	allNamespaces := false
	args := fields[2:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if cmd.resource == "" {
				cmd.resource = arg
			}
			continue
		}

		flag, value, hasValue := splitFlag(arg)
		switch flag {
		case "-o", "--output":
			if !hasValue {
				if i+1 == len(args) {
					return getCommand{}, false
				}
				i++
				value = args[i]
			}
			// `wide` is the same table with more columns, so the hint says the
			// same thing about it; every other value is an explicit format that
			// already printed the object, or the projection the user asked for.
			if value != "table" && value != "wide" {
				return getCommand{}, false
			}
		case "-n", "--namespace":
			if !hasValue {
				if i+1 == len(args) {
					return getCommand{}, false
				}
				i++
				value = args[i]
			}
			cmd.namespace = value
		case "-A", "--all-namespaces":
			allNamespaces = true
		default:
			if !hasValue && getFlagsWithValue[flag] && i+1 < len(args) {
				i++
			}
		}
	}

	if cmd.resource == "" {
		return getCommand{}, false
	}
	// `-A` wins over `-n` whatever order they were written in, as it does in
	// kubectl, and an empty namespace asks the permission API the cluster-scoped
	// question — the one a command reading every namespace is asking.
	if allNamespaces {
		cmd.namespace = ""
	}
	cmd.canonical = canonicalResource(cmd.resource)
	return cmd, true
}

// splitFlag splits a field into its flag and, when the value is written into the
// same field, that value (`--output=yaml`, `-oyaml`, `-o=yaml`).
func splitFlag(arg string) (flag, value string, hasValue bool) {
	if strings.HasPrefix(arg, "--") {
		if eq := strings.Index(arg, "="); eq >= 0 {
			return arg[:eq], arg[eq+1:], true
		}
		return arg, "", false
	}
	// A short flag carries its value directly: -oyaml, -nns, -o=yaml.
	if len(arg) > 2 {
		return arg[:2], strings.TrimPrefix(arg[2:], "="), true
	}
	return arg, "", false
}

// canonicalResource reduces what the user typed to the plural RBAC name, or to
// "" for a resource this hint has nothing to say about. A group suffix is
// dropped because the permission API authorizes the core group only, so a
// resource in another group cannot be confirmed and gets no second line.
func canonicalResource(name string) string {
	name = strings.ToLower(name)
	if dot := strings.Index(name, "."); dot >= 0 {
		name = name[:dot]
	}
	return resourceAliases[name]
}

// resourcesPermission is the body of the platform's batch permission API.
type resourcesPermission struct {
	Cluster string               `json:"cluster"`
	Infos   []resourcePermission `json:"infos"`
}

type resourcePermission struct {
	Resource  string `json:"resource"`
	Operator  string `json:"operator"`
	Namespace string `json:"namespace"`
}

// sendGetHint sends the hint for one finished command line, or nothing. It is
// called in its own goroutine: the keystroke has to reach the pod before the
// platform is asked anything.
func (t TerminalSession) sendGetHint(line string) {
	// The session is built with its connect info before Read is ever called; the
	// guard is here because a panic in this goroutine would take the gateway down
	// with it, and a missing hint is a far smaller failure than that.
	if t.cInfo == nil {
		return
	}

	cmd, ok := parseKubectlGet(line)
	if !ok {
		return
	}

	// Only a resource with a related set needs the platform's answer; the rest of
	// the hint is a statement about tables, not about this user's rights. When the
	// answer does not come the whole hint is dropped rather than guessed, because
	// naming a resource the user cannot read is worse than saying nothing.
	related := relatedResources[cmd.canonical]
	var allowed map[string]bool
	if len(related) > 0 {
		allowed = t.allowedResources(t.cInfo.ClusterName, cmd.namespace, related)
		if allowed == nil {
			return
		}
	}

	hint := fmt.Sprintf(hintSummary, cmd.resource)
	if readable := readableResources(related, allowed); len(readable) > 0 {
		// The line break is written as CRLF: the terminal is in raw mode, where a
		// bare LF moves down a line without returning to the first column.
		hint += "\r\n" + fmt.Sprintf(hintRelated, strings.Join(readable, ","), cmd.namespaceFlag())
	}

	msg, err := json.Marshal(TerminalMessage{Op: "toast", Data: hint})
	if err != nil {
		clog.Error("[%v] marshal toast message failed: %v", t.id, err)
		return
	}
	if err := t.sockJSSession.Send(string(msg)); err != nil {
		clog.Warn("[%v] send toast message failed: %v", t.id, err)
	}
}

// allowedResources asks the platform whether this session's user may read each
// resource in the given namespace. It returns only the resources the platform
// confirmed, and nil for every failure — a transport error, a non-200, a body
// that is not the expected map — so the caller sends nothing rather than a hint
// it cannot stand behind.
func (t TerminalSession) allowedResources(cluster, namespace string, resources []string) map[string]bool {
	infos := make([]resourcePermission, 0, len(resources))
	for _, r := range resources {
		// `read` is what the console sends for a listing permission; the API sorts
		// it up to the `get` verb itself.
		infos = append(infos, resourcePermission{Resource: r, Operator: "read", Namespace: namespace})
	}

	payload, err := json.Marshal(resourcesPermission{Cluster: cluster, Infos: infos})
	if err != nil {
		clog.Error("[%v] marshal resources permission request failed: %v", t.id, err)
		return nil
	}

	req, err := http.NewRequest(http.MethodPost, utils.GetKubeCubeSvc()+authorizationResourcesPath, bytes.NewReader(payload))
	if err != nil {
		clog.Error("[%v] build resources permission request failed: %v", t.id, err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	// The API authorizes the caller of the request, and the gateway holds no
	// identity of its own: the browser's own credential has to travel with it. A
	// session that carried none — a container exec — is answered 401, which is
	// this function's ordinary "no".
	for _, h := range []string{"Cookie", "Authorization"} {
		if v := t.cInfo.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := permissionHTTPClient.Do(req)
	if err != nil {
		clog.Warn("[%v] ask resources permission failed: %v", t.id, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		clog.Warn("[%v] ask resources permission answered status %d", t.id, resp.StatusCode)
		return nil
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		clog.Warn("[%v] read resources permission response failed: %v", t.id, err)
		return nil
	}

	allowed := make(map[string]bool)
	if err := json.Unmarshal(body, &allowed); err != nil {
		clog.Warn("[%v] parse resources permission response failed: %v", t.id, err)
		return nil
	}
	return allowed
}

// readableResources keeps the candidates in the order they were declared, so the
// suggestion reads the way the resources explain each other.
func readableResources(candidates []string, allowed map[string]bool) []string {
	var readable []string
	for _, r := range candidates {
		if allowed[r] {
			readable = append(readable, r)
		}
	}
	return readable
}
