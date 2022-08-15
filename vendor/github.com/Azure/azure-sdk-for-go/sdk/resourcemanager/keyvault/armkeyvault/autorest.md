### AutoRest Configuration

> see https://aka.ms/autorest

``` yaml
azure-arm: true
tag: package-2021-10
require:
- https://github.com/Azure/azure-rest-api-specs/blob/0cc5e2efd6ffccf30e80d1e150b488dd87198b94/specification/keyvault/resource-manager/readme.md
- https://github.com/Azure/azure-rest-api-specs/blob/0cc5e2efd6ffccf30e80d1e150b488dd87198b94/specification/keyvault/resource-manager/readme.go.md
license-header: MICROSOFT_MIT_NO_VERSION
module-version: 1.0.0
modelerfour:
  seal-single-value-enum-by-default: true
```