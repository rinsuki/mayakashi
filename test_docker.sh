#!/bin/bash
docker build -t mayakashi-test . && docker run --rm --device /dev/fuse --cap-add SYS_ADMIN -e PYTHONUNBUFFERED=1 mayakashi-test