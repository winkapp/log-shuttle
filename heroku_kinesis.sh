#!/bin/bash

env | grep KINESIS

socat tcp4-listen:514,fork - | /bin/log-shuttle \
  -logs-url "$KINESIS_URL" \
  -max-line-length 32000 \
  -batch-size 150 \
  -input-format rfc5424 \
  -kinesis-shards "$KINESIS_SHARDS" \
  -kinesis-parition-key "$KINESIS_PARTITION_KEY" \
  -verbose
