# End-to-end tests

The Java extension ImageVolume test is opt-in because it requires a Kubernetes
1.35 Kind node. Build an extension image locally, then run:

```sh
E2E_JAVA_EXTENSION_IMAGE=true \
JAVA_EXTENSION_IMAGE=java-extension-app:dev \
JAVA_EXTENSION_MARKER=JAVA_EXTENSION_CUSTOMIZER_APPLIED \
mise exec go@1.26.3 -- go test -tags=e2e ./test/e2e/... -v \
  -ginkgo.focus='Java extension image volume' -timeout 30m
```

The suite builds and loads `java-extension-app:dev` into Kind; the image is
used both as the test workload and as the extension image so the fixture stays
self-contained. This test verifies that admission adds the configured
`ImageVolumeSource` and passes its Java extension path to the injector through
`OTEL_INJECTOR_JAVAAGENT_EXTENSIONS`. The injector appends these paths to the
process's `OTEL_JAVAAGENT_EXTENSIONS`, preserving any value supplied directly
by the workload. It also verifies that an instrumented Java HTTP workload
starts and that the extension's `AutoConfigurationCustomizerProvider` runs
inside the upstream Java agent.

The runtime assertion also requires the SDK image referenced by
`sdkImageVersion` to include OTel Injector support for
`OTEL_INJECTOR_JAVAAGENT_EXTENSIONS` (proposed in [upstream PR #452](https://github.com/open-telemetry/opentelemetry-injector/pull/452)).
The currently pinned SDK image predates that support; update it after a
compatible injector build is available before running this test.
