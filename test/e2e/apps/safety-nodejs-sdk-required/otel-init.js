'use strict';
// This file is required via CMD --require to simulate an app that loads an
// OTel-SDK-dependent module at startup. register.js detects @opentelemetry/sdk-node
// in this app's package.json via isOtelSdkRequiredViaArgs and skips injection.
