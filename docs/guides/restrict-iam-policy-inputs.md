# Restrict IAM Policy Inputs

`AWSServiceAccountRole` is where workloads request IAM permissions through
`spec.policyARNs` or `spec.policyDocument`. The operator validates API shape and
generates IAM role and trust policy resources, but it does not keep a
platform-specific allowlist or inspect inline policy semantics.

If your platform needs to restrict requested IAM permissions, enforce that
policy at admission time. For example, this Kyverno policy allows only approved
managed policy ARNs and disables inline policy documents:

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: aws-workload-identity-policy-inputs
spec:
  background: false
  rules:
    - name: allow-approved-managed-policy-arns
      match:
        any:
          - resources:
              kinds:
                - aws.identity.appthrust.io/v1alpha1/AWSServiceAccountRole
              operations:
                - CREATE
                - UPDATE
      validate:
        failureAction: Enforce
        message: spec.policyARNs may only use platform-approved managed policy ARNs.
        deny:
          conditions:
            any:
              - key: "{{ request.object.spec.policyARNs[] || [] }}"
                operator: AnyNotIn
                value:
                  - arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
                  - arn:aws:iam::123456789012:policy/platform-approved-app-policy

    - name: block-inline-policy-documents
      match:
        any:
          - resources:
              kinds:
                - aws.identity.appthrust.io/v1alpha1/AWSServiceAccountRole
              operations:
                - CREATE
                - UPDATE
      validate:
        failureAction: Enforce
        message: spec.policyDocument is disabled on this platform. Use approved managed policies.
        deny:
          conditions:
            any:
              - key: "{{ request.object.spec.policyDocument || '' }}"
                operator: NotEquals
                value: ""
```
