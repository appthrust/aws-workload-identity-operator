import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const chartPath = "charts/aws-workload-identity-operator";

function helmTemplate(values?: string): string {
  const args = ["template", "awio", chartPath, "--namespace", "awio-system", "--hide-notes"];
  let tempDir = "";
  if (values !== undefined) {
    tempDir = mkdtempSync(join(tmpdir(), "awio-kest-"));
    const valuesFile = join(tempDir, "values.yaml");
    writeFileSync(valuesFile, values);
    args.push("--values", valuesFile);
  }

  try {
    const result = spawnSync("helm", args, { encoding: "utf8" });
    if (result.status !== 0) {
      throw new Error(result.stderr + result.stdout);
    }

    return result.stdout;
  } finally {
    if (tempDir !== "") {
      rmSync(tempDir, { recursive: true, force: true });
    }
  }
}

function helmTemplateFailure(values: string): string {
  const tempDir = mkdtempSync(join(tmpdir(), "awio-kest-"));
  try {
    const valuesFile = join(tempDir, "values.yaml");
    writeFileSync(valuesFile, values);
    const result = spawnSync(
      "helm",
      ["template", "awio", chartPath, "--namespace", "awio-system", "--hide-notes", "--values", valuesFile],
      { encoding: "utf8" },
    );
    expect(result.status).not.toBe(0);
    return result.stderr + result.stdout;
  } finally {
    rmSync(tempDir, { recursive: true, force: true });
  }
}

function providerChecksum(manifest: string): string {
  const match = manifest.match(/checksum\/clusterprofile-provider-file: ([a-f0-9]+)/);
  expect(match).not.toBeNull();
  return match?.[1] ?? "";
}

describe("manifest shape", () => {
  test("manager uses image volumes for ClusterProfile access plugin", () => {
    const manifest = readFileSync("config/manager/manager.yaml", "utf8");
    expect(manifest).toContain("--clusterprofile-provider-file=/etc/cluster-inventory/config.json");
    expect(manifest).toContain("open-cluster-management");
    expect(manifest).toContain("image:");
    expect(manifest).toContain("quay.io/open-cluster-management/cp-creds");
    expect(manifest).toContain("secretreader");
    expect(manifest).toContain("kubeconfig-secretreader");
  });

  test("CRD keeps immutable delivery inputs in CEL", () => {
    const configCRD = readFileSync(
      "config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml",
      "utf8",
    );
    expect(configCRD).toContain("self == oldSelf");
    expect(configCRD).toContain("SelfHostedIRSA");
    expect(configCRD).toContain("EKSPodIdentity");
  });

  test("CRD types workload status ACK resources", () => {
    const configCRD = readFileSync(
      "config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml",
      "utf8",
    );
    const roleCRD = readFileSync(
      "config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml",
      "utf8",
    );

    for (const crd of [configCRD, roleCRD]) {
      expect(crd).toContain("controller-gen.kubebuilder.io/version:");
      expect(crd).toContain("ackResources:");
      expect(crd).toContain("x-kubernetes-list-map-keys:");
      expect(crd).toContain("- apiVersion");
      expect(crd).toContain("- kind");
      expect(crd).toContain("- namespace");
      expect(crd).toContain("- name");
      expect(crd).toContain("lastTransitionTime:");
      expect(crd).not.toContain("status:\n            type: object\n            x-kubernetes-preserve-unknown-fields: true");
    }
  });

  test("operator config CRD keeps permissions boundary optional", () => {
    const configCRD = readFileSync(
      "config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml",
      "utf8",
    );

    expect(configCRD).toContain("permissionsBoundaryARN:");
    expect(configCRD).not.toContain("- permissionsBoundaryARN");
  });

  test("chart CRDs stay in sync with config CRDs", () => {
    for (const name of [
      "aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml",
      "aws.identity.appthrust.io_awsserviceaccountroles.yaml",
      "aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml",
    ]) {
      expect(readFileSync(`charts/aws-workload-identity-operator/templates/crds/${name}`, "utf8")).toBe(
        readFileSync(`config/crd/bases/${name}`, "utf8"),
      );
    }
  });

  test("chart keeps probes and resource defaults enabled", () => {
    const deployment = readFileSync(
      "charts/aws-workload-identity-operator/templates/deployment.yaml",
      "utf8",
    );
    const rbac = readFileSync("charts/aws-workload-identity-operator/templates/rbac.yaml", "utf8");
    const values = readFileSync("charts/aws-workload-identity-operator/values.yaml", "utf8");
    expect(deployment).toContain("livenessProbe:");
    expect(deployment).toContain("readinessProbe:");
    expect(deployment).not.toContain(".Values.livenessProbe.enabled");
    expect(deployment).not.toContain(".Values.readinessProbe.enabled");
    expect(deployment).toContain("resources:");
    expect(values).toContain("requests:");
    expect(values).toContain("cpu: 100m");
    expect(values).toContain("memory: 128Mi");
    expect(values).toContain("limits:");
    expect(values).toContain("memory: 256Mi");
    expect(values).not.toContain("resources: {}");
  });

  test("chart keeps RBAC and metrics endpoint always enabled", () => {
    const deployment = readFileSync(
      "charts/aws-workload-identity-operator/templates/deployment.yaml",
      "utf8",
    );
    const rbac = readFileSync("charts/aws-workload-identity-operator/templates/rbac.yaml", "utf8");
    const values = readFileSync("charts/aws-workload-identity-operator/values.yaml", "utf8");

    expect(deployment).toContain("--metrics-bind-address=:%d");
    expect(deployment).toContain("name: metrics");
    expect(deployment).not.toContain(".Values.metrics.enabled");
    expect(deployment).not.toContain(".Values.metrics.bindAddress");
    expect(rbac).not.toContain(".Values.rbac.create");
    expect(values).not.toContain("bindAddress:");
  });

  test("chart keeps webhook operational knobs internal", () => {
    const deployment = readFileSync(
      "charts/aws-workload-identity-operator/templates/deployment.yaml",
      "utf8",
    );
    const webhook = readFileSync("charts/aws-workload-identity-operator/templates/webhook.yaml", "utf8");
    const service = readFileSync(
      "charts/aws-workload-identity-operator/templates/service-webhook.yaml",
      "utf8",
    );
    const values = readFileSync("charts/aws-workload-identity-operator/values.yaml", "utf8");
    const schema = JSON.parse(
      readFileSync("charts/aws-workload-identity-operator/values.schema.json", "utf8"),
    );

    expect(webhook).not.toContain(".Values.webhook");
    expect(deployment).not.toContain(".Values.webhook.enabled");
    expect(webhook).not.toContain(".Values.webhook.failurePolicy");
    expect(webhook).not.toContain(".Values.webhook.admissionReviewVersions");
    expect(webhook).not.toContain(".Values.webhook.namespaceSelector");
    expect(service).not.toContain(".Values.webhook.service");
    expect(service).not.toContain(".Values.webhook");
    expect(schema.required).not.toContain("webhook");
    expect(schema.properties.webhook).toBeUndefined();
    expect(values).not.toContain("certManager:");
    expect(values).not.toContain("failurePolicy:");
    expect(values).not.toContain("admissionReviewVersions:");
    expect(values).not.toContain("objectSelector:");
  });

  test("self-hosted remote delivery uses ServiceAccount annotations only", () => {
    const manager = readFileSync("cmd/manager/main.go", "utf8");
    const deployment = readFileSync(
      "charts/aws-workload-identity-operator/templates/deployment.yaml",
      "utf8",
    );
    const runtime = readFileSync(
      "internal/controller/selfhosted_webhook_runtime_controller.go",
      "utf8",
    );
    const serviceAccount = readFileSync(
      "internal/controller/selfhosted_serviceaccount_controller.go",
      "utf8",
    );
    const delivery = readFileSync("internal/controller/selfhosted_annotations.go", "utf8");
    const conditions = readFileSync("api/v1alpha1/condition_types.go", "utf8");
    const configController = readFileSync("internal/controller/config_controller.go", "utf8");
    const oidcDocuments = readFileSync("internal/oidc/documents.go", "utf8");

    expect(manager).toContain("SetupSelfHostedServiceAccountController");
    expect(runtime).toContain('Resources: []string{"serviceaccounts"}');
    expect(runtime).toContain("--annotation-prefix=");
    expect(delivery).toContain('selfHostedAnnotationPrefix = "eks.amazonaws.com"');
    expect(serviceAccount).toContain("patchRemoteServiceAccountAnnotations");
    expect(delivery).toContain("renderSelfHostedServiceAccountAnnotations");
    expect(conditions).toContain("ConditionServiceAccountAnnotationReady");
    expect(conditions).toContain("ConditionOIDCObjectsPublished");
    expect(configController).toContain("PublishOIDCIssuer");
    expect(oidcDocuments).toContain('DiscoveryObjectKey = ".well-known/openid-configuration"');
    expect(oidcDocuments).toContain('JWKSObjectKey = "keys.json"');
    expect(manager).toContain('"aws-endpoint-url"');
    expect(manager).toContain('"allow-unsafe-aws-endpoint-urls"');
    expect(deployment).toContain("--aws-endpoint-url");
    expect(deployment).toContain("AWS_ENDPOINT_URL");
    expect(deployment).toContain("--allow-unsafe-aws-endpoint-urls");
  });

  test("chart validates values through values.schema.json", () => {
    const schema = JSON.parse(
      readFileSync("charts/aws-workload-identity-operator/values.schema.json", "utf8"),
    );
    const rbac = readFileSync("charts/aws-workload-identity-operator/templates/rbac.yaml", "utf8");
    const webhook = readFileSync("charts/aws-workload-identity-operator/templates/webhook.yaml", "utf8");
    const accessProvidersTemplate = readFileSync(
      "charts/aws-workload-identity-operator/templates/access-providers-config.yaml",
      "utf8",
    );
    const helpers = readFileSync("charts/aws-workload-identity-operator/templates/_helpers.tpl", "utf8");

    const resources = schema.definitions.resources;
    expect(schema.properties.resources.$ref).toBe("#/definitions/resources");
    expect(schema.required).toContain("aws");
    expect(schema.required).toContain("ocm");
    expect(schema.properties.aws.properties.endpointURL.type).toBe("string");
    expect(schema.properties.aws.properties.allowUnsafeEndpointURLs.type).toBe("boolean");
    expect(schema.properties.aws.allOf[0].then.properties.allowUnsafeEndpointURLs.const).toBe(true);
    expect(schema.properties.clusterInventory.required).toContain("accessProviders");
    expect(schema.properties.clusterInventory.properties.accessProviders.items.$ref).toBe("#/definitions/accessProvider");
    expect(schema.properties.clusterInventory.properties.accessProviders.minItems).toBeUndefined();
    expect(schema.properties.ocm.properties.managedServiceAccount.properties.name.$ref).toBe("#/definitions/dnsLabel");
    expect(schema.properties.ocm.properties.managedServiceAccount.properties.namespaces.uniqueItems).toBe(true);
    expect(schema.properties.ocm.properties.managedServiceAccount.properties.addonInstallNamespace.$ref).toBe("#/definitions/dnsLabel");
    expect(schema.properties.ocm.properties.managedServiceAccount.properties.remotePermissions.properties.webhookNamespace.$ref).toBe(
      "#/definitions/optionalDnsLabel",
    );
    expect(schema.properties.ocm.properties.managedServiceAccount.properties.remotePermissions.required).toEqual([
      "name",
      "webhookNamespace",
    ]);
    expect(schema.properties.serviceAccount.required).toContain("name");
    expect(schema.properties.serviceAccount.properties.name.$ref).toBe("#/definitions/optionalDnsLabel");
    expect(resources.required).toEqual(["requests", "limits"]);
    expect(resources.properties.requests.required).toContain("cpu");
    expect(resources.properties.requests.required).toContain("memory");
    expect(resources.properties.limits.required).toEqual(["memory"]);
    expect(schema.properties.operatorConfig.allOf[0].then.properties.spec.minProperties).toBe(1);
    expect(webhook).toContain("cert-manager.io/v1");
    expect(webhook).toContain("cert-manager.io/inject-ca-from");
    expect(webhook).not.toContain("MutatingWebhookConfiguration");
    expect(rbac).toContain("resources:\n      - secrets\n    verbs:\n      - get\n      - list\n      - watch");
    expect(accessProvidersTemplate).toContain('include "awio.clusterInventoryProviderConfigJSON"');
    expect(helpers).toContain("does not live under");
    expect(helpers).toContain("duplicate Cluster Inventory access-provider name");
  });

  test("chart renders Cluster Inventory providers from clusterInventory values", () => {
    const values = readFileSync("charts/aws-workload-identity-operator/values.yaml", "utf8");
    const manifest = helmTemplate();

    expect(values).toContain("accessProviders:");
    expect(manifest).toContain("--managed-serviceaccount=aws-workload-identity-operator");
    expect(manifest).not.toContain("kind: ManagedServiceAccount");

    const ocmOnlyManifest = helmTemplate(`
ocm:
  managedServiceAccount:
    name: custom-awio
`);
    expect(ocmOnlyManifest).toContain("--managed-serviceaccount=custom-awio");
    expect(ocmOnlyManifest).not.toContain("--managed-serviceaccount=aws-workload-identity-operator");
    expect(providerChecksum(ocmOnlyManifest)).not.toBe(providerChecksum(manifest));

    const mergedManifest = helmTemplate(`
clusterInventory:
  accessProviders:
    - name: secondary
      execConfig:
        apiVersion: client.authentication.k8s.io/v1
        command: /extra/secondary/cp-creds
        provideClusterInfo: true
        interactiveMode: Never
  plugins:
    - name: open-cluster-management
      image: quay.io/open-cluster-management/cp-creds:latest
      mountPath: /plugins
      pullPolicy: IfNotPresent
    - name: secondary
      image: quay.io/open-cluster-management/cp-creds:latest
      mountPath: /extra/secondary
      pullPolicy: IfNotPresent
`);

    expect(mergedManifest).toContain("open-cluster-management");
    expect(mergedManifest).toContain("secondary");
    expect(mergedManifest).toContain("--managed-serviceaccount=aws-workload-identity-operator");
  });

  test("chart can render OCM ManagedServiceAccount and remote permissions", () => {
    const manifest = helmTemplate(`
namespaceOverride: operator-ns
ocm:
  managedServiceAccount:
    name: custom-awio
    create: true
    namespaces:
      - wlc-a
    addonInstallNamespace: custom-msa-install
    labels:
      authentication.open-cluster-management.io/sync-to-clusterprofile: "false"
    remotePermissions:
      name: custom-awio-remote
      webhookNamespace: custom-webhook
`);

    expect(manifest).toContain("kind: ManagedServiceAccount");
    expect(manifest).toContain("name: custom-awio");
    expect(manifest).toContain("namespace: wlc-a");
    expect(manifest).toContain('authentication.open-cluster-management.io/sync-to-clusterprofile: "true"');
    expect(manifest).not.toContain('authentication.open-cluster-management.io/sync-to-clusterprofile: "false"');
    expect(manifest).toContain("kind: ManifestWork");
    expect(manifest).toContain("name: custom-awio-remote");
    expect(manifest).toContain("namespace: custom-webhook");
    expect(manifest).toContain("namespace: custom-msa-install");
    expect(manifest).not.toContain("name: cluster-admin");
    expect(manifest).not.toContain("kind: ManagedServiceAccount\nmetadata:\n  name: custom-awio\n  namespace: operator-ns");
  });

  test("chart rejects invalid provider and OCM resource combinations", () => {
    expect(
      helmTemplateFailure(`
clusterInventory:
  accessProviders:
    - name: duplicate
      execConfig:
        command: /plugins/cp-creds
    - name: duplicate
      execConfig:
        command: /plugins/cp-creds
`),
    ).toContain('duplicate Cluster Inventory access-provider name "duplicate"');

    expect(
      helmTemplateFailure(`
clusterInventory:
  accessProviders:
    - name: open-cluster-management
      execConfig:
        command: /plugins/cp-creds
`),
    ).toContain('duplicate Cluster Inventory access-provider name "open-cluster-management"');

    expect(
      helmTemplateFailure(`
ocm:
  managedServiceAccount:
    create: true
    namespaces:
      - wlc-a
      - wlc-a
`),
    ).toContain("namespaces");
  });
});
