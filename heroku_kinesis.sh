#!/bin/bash

env | grep KINESIS

socat UDP4-RECVFROM:514,fork - | tee /dev/stderr | /bin/log-shuttle \
  -logs-url "$KINESIS_URL" \
  -max-line-length 32000 \
  -batch-size 150 \
  -input-format rfc5424 \
  -kinesis-shards "$KINESIS_SHARDS" \
  -verbose
