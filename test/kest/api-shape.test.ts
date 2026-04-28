import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";

describe("manifest shape", () => {
  test("manager uses image volumes for ClusterProfile access plugin", () => {
    const manifest = readFileSync("config/manager/manager.yaml", "utf8");
    expect(manifest).not.toContain("--inventory-namespace");
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

  test("operator config CRD supports optional boundary without policy filters", () => {
    const configCRD = readFileSync(
      "config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml",
      "utf8",
    );

    expect(configCRD).toContain("permissionsBoundaryARN:");
    expect(configCRD).not.toContain("policyGuardrails:");
    expect(configCRD).not.toContain("managedPolicyARNRules:");
    expect(configCRD).not.toContain("allowedManagedPolicyARNs:");
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
    const values = readFileSync("charts/aws-workload-identity-operator/values.yaml", "utf8");
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
    expect(manager).not.toContain(["s3", "endpoint-url"].join("-"));
    expect(deployment).toContain("--aws-endpoint-url");
    expect(deployment).toContain("AWS_ENDPOINT_URL");
    expect(deployment).toContain("--allow-unsafe-aws-endpoint-urls");
    expect(deployment).toContain("--s3-use-path-style");
    expect(deployment).not.toContain("--selfhosted-webhook-namespace");
    expect(values).toContain("endpointURL:");
    expect(values).toContain("allowUnsafeEndpointURLs:");
    expect(values).toContain("usePathStyle:");
    expect(values).not.toContain("selfHostedWebhookNamespace:");
  });

  test("chart validates values through values.schema.json", () => {
    const schema = JSON.parse(
      readFileSync("charts/aws-workload-identity-operator/values.schema.json", "utf8"),
    );
    const deployment = readFileSync(
      "charts/aws-workload-identity-operator/templates/deployment.yaml",
      "utf8",
    );
    const rbac = readFileSync("charts/aws-workload-identity-operator/templates/rbac.yaml", "utf8");
    const operatorConfig = readFileSync(
      "charts/aws-workload-identity-operator/templates/operatorconfig.yaml",
      "utf8",
    );
    const webhook = readFileSync("charts/aws-workload-identity-operator/templates/webhook.yaml", "utf8");
    const serviceMonitor = readFileSync(
      "charts/aws-workload-identity-operator/templates/servicemonitor.yaml",
      "utf8",
    );
    const accessProvidersConfig = readFileSync(
      "charts/aws-workload-identity-operator/templates/access-providers-config.yaml",
      "utf8",
    );
    const admission = readFileSync("internal/controller/admission.go", "utf8");

    const resources = schema.definitions.resources;
    expect(schema.properties.resources.$ref).toBe("#/definitions/resources");
    expect(schema.required).toContain("aws");
    expect(schema.required).toContain("s3");
    expect(schema.properties.aws.properties.endpointURL.type).toBe("string");
    expect(schema.properties.aws.properties.allowUnsafeEndpointURLs.type).toBe("boolean");
    expect(schema.properties.aws.allOf[0].then.properties.allowUnsafeEndpointURLs.const).toBe(true);
    expect(schema.properties.s3.properties.usePathStyle.type).toBe("boolean");
    expect(schema.properties.operator.required).not.toContain("selfHostedWebhookNamespace");
    expect(schema.properties.operator.properties.selfHostedWebhookNamespace).toBeUndefined();
    expect(resources.required).toEqual(["requests", "limits"]);
    expect(resources.properties.requests.required).toContain("cpu");
    expect(resources.properties.requests.required).toContain("memory");
    expect(resources.properties.limits.required).toEqual(["memory"]);
    expect(schema.properties.operatorConfig.allOf[0].then.properties.spec.minProperties).toBe(1);
    expect(deployment).not.toContain("required \"clusterInventory.plugins[].image is required\"");
    expect(operatorConfig).not.toContain("operatorConfig.spec is required");
    expect(webhook).toContain("cert-manager.io/v1");
    expect(webhook).toContain("cert-manager.io/inject-ca-from");
    expect(webhook).not.toContain("MutatingWebhookConfiguration");
    expect(deployment).not.toContain("--log-stacktrace-level");
    expect(rbac).toContain("resources:\n      - secrets\n    verbs:\n      - get\n      - list\n      - watch");
    expect(webhook).not.toContain("/mutate-aws-identity-appthrust-io-v1alpha1-awsworkloadidentityoperatorconfig");
    expect(serviceMonitor).not.toContain("serviceMonitor.enabled requires");
    expect(accessProvidersConfig).toContain("does not live under");
    expect(admission).not.toContain("BasicValidateConfig");
    expect(admission).not.toContain("BasicValidateRole");
    expect(admission).not.toContain("BasicValidateOperatorConfig");
    expect(admission).not.toContain("spec.serviceAccount is immutable");
    expect(admission).not.toContain("spec.type is immutable");
    expect(admission).not.toContain("spec.region is immutable");
  });
});
