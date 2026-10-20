package randomvalues

import (
	"slices"
	"strings"
	"testing"
)

const placeholder = "CONSCIOUSLY_NOT_RENDERED"

const firstRender = `---
# Source: webhook/templates/secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: webhook-tls
data:
  ca.crt: "Q0Ex"
  tls.key: S0VZMQ== # key
  static: c3RhdGlj
---
# Source: webhook/templates/webhook.yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: webhook
webhooks:
- name: a
  clientConfig:
    caBundle: Q0Ex
- name: b
  clientConfig:
    caBundle: Q0Ex
---
# Source: webhook/templates/configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: certs
data:
  ca.pem: |
    -----BEGIN CERTIFICATE-----
    one
    -----END CERTIFICATE-----
  after: kept
`

var secondRender = strings.NewReplacer(
	`"Q0Ex"`, `"Q0Ey"`,
	`S0VZMQ==`, `S0VZMg==`,
	`caBundle: Q0Ex`, `caBundle: Q0Ey`,
	"    one\n", "    two\n",
).Replace(firstRender)

func TestMaskReplacesValuesThatDiffer(t *testing.T) {
	got, fields, err := Mask([]byte(firstRender), []byte(secondRender), placeholder)

	if err != nil {
		t.Fatal(err)
	}
	want := `---
# Source: webhook/templates/secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: webhook-tls
data:
  ca.crt: "CONSCIOUSLY_NOT_RENDERED"
  tls.key: CONSCIOUSLY_NOT_RENDERED # key
  static: c3RhdGlj
---
# Source: webhook/templates/webhook.yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: webhook
webhooks:
- name: a
  clientConfig:
    caBundle: CONSCIOUSLY_NOT_RENDERED
- name: b
  clientConfig:
    caBundle: CONSCIOUSLY_NOT_RENDERED
---
# Source: webhook/templates/configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: certs
data:
  ca.pem: CONSCIOUSLY_NOT_RENDERED
  after: kept
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	wantFields := []string{
		`v1 Secret webhook-tls .data["ca.crt"]`,
		`v1 Secret webhook-tls .data["tls.key"]`,
		`admissionregistration.k8s.io/v1 MutatingWebhookConfiguration webhook .webhooks[0].clientConfig.caBundle`,
		`admissionregistration.k8s.io/v1 MutatingWebhookConfiguration webhook .webhooks[1].clientConfig.caBundle`,
		`v1 ConfigMap certs .data["ca.pem"]`,
	}
	if !slices.Equal(fields, wantFields) {
		t.Errorf("Expected masked fields %q, got %q", wantFields, fields)
	}
}

func TestMaskLeavesIdenticalRendersAlone(t *testing.T) {
	got, fields, err := Mask([]byte(firstRender), []byte(firstRender), placeholder)

	if err != nil {
		t.Fatal(err)
	}
	if string(got) != firstRender || len(fields) != 0 {
		t.Errorf("Expected the manifest to be unchanged, got fields %q and:\n%s", fields, got)
	}
}

func TestMaskRejectsRendersWithDifferentDocuments(t *testing.T) {
	second := strings.Replace(firstRender, "name: certs", "name: certs-x7k2p", 1)

	_, _, err := Mask([]byte(firstRender), []byte(second), placeholder)

	if err == nil || !strings.Contains(err.Error(), "v1 ConfigMap certs and v1 ConfigMap certs-x7k2p") {
		t.Errorf("Expected an error naming both documents, got: %v", err)
	}
}

func TestMaskRejectsRendersWithDifferentNumbersOfDocuments(t *testing.T) {
	second := firstRender[:strings.LastIndex(firstRender, "\n---\n")+1]

	_, _, err := Mask([]byte(firstRender), []byte(second), placeholder)

	if err == nil || !strings.Contains(err.Error(), "renders have 3 and 2 documents") {
		t.Errorf("Expected an error about the number of documents, got: %v", err)
	}
}

func TestMaskRejectsDocumentsWithDifferentStructure(t *testing.T) {
	second := strings.Replace(secondRender, "- name: b\n  clientConfig:\n    caBundle: Q0Ey\n", "", 1)

	_, _, err := Mask([]byte(firstRender), []byte(second), placeholder)

	if err == nil || !strings.Contains(err.Error(), "admissionregistration.k8s.io/v1 MutatingWebhookConfiguration webhook: .webhooks differs in structure") {
		t.Errorf("Expected an error about .webhooks, got: %v", err)
	}
}

func TestMaskRejectsDocumentsInDifferentNamespaces(t *testing.T) {
	document := func(namespace, token string) string {
		return "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: token\n  namespace: " + namespace + "\ndata:\n  token: " + token + "\n"
	}
	first := document("a", "one") + document("b", "two")
	second := document("b", "three") + document("a", "four")

	_, _, err := Mask([]byte(first), []byte(second), placeholder)

	if err == nil || !strings.Contains(err.Error(), "v1 Secret a/token and v1 Secret b/token") {
		t.Errorf("Expected an error naming both documents, got: %v", err)
	}
}

func TestMaskReplacesValuesInEveryStyle(t *testing.T) {
	render := func(value string) string {
		return strings.NewReplacer("VALUE", value).Replace(`apiVersion: v1
kind: ConfigMap
metadata:
  name: styles
data:
  anchored: &shared VALUE
  alias: *shared
  tagged: !!str VALUE
  plain: VALUE
    continued
  quoted: "VALUE
    continued"
  list: [VALUE, kept]
`)
	}

	got, fields, err := Mask([]byte(render("first")), []byte(render("second")), placeholder)

	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: v1
kind: ConfigMap
metadata:
  name: styles
data:
  anchored: &shared CONSCIOUSLY_NOT_RENDERED
  alias: *shared
  tagged: CONSCIOUSLY_NOT_RENDERED
  plain: CONSCIOUSLY_NOT_RENDERED
  quoted: "CONSCIOUSLY_NOT_RENDERED"
  list: [CONSCIOUSLY_NOT_RENDERED, kept]
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	wantFields := []string{
		"v1 ConfigMap styles .data.anchored",
		"v1 ConfigMap styles .data.tagged",
		"v1 ConfigMap styles .data.plain",
		"v1 ConfigMap styles .data.quoted",
		"v1 ConfigMap styles .data.list[0]",
	}
	if !slices.Equal(fields, wantFields) {
		t.Errorf("Expected masked fields %q, got %q", wantFields, fields)
	}
}
