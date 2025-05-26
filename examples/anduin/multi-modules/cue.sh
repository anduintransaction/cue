#!/bin/env bash

here=`cd $(dirname $BASH_SOURCE); pwd`

export ANDUIN_CUE_DEBUG="true"
export CUE_CACHE_DIR=$here/cue.cache/

: "${USE_ZOT:=false}"
if [ "${USE_ZOT}" = "true" ]; then
  export CUE_REGISTRY=file://$here/zot.cue
else
  export CUE_REGISTRY=file://$here/registry.cue
  export CUE_CONFIG_DIR=$here/cue.config/
fi

cue $@
