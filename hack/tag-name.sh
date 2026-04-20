#!/bin/sh
#
# Compute the release tag for a given Python patch version.
#
#   v<lib>                        for the primary Python line (VERSION + PRIMARY_PYTHON)
#   v<lib>-py<python-version>     for every other Python line
#
# The primary tag has no prerelease suffix so `go get @latest` resolves to it.
# Non-primary tags are valid semver prereleases of the primary tag, so users
# pin them explicitly.

set -e

DIR=$(cd $(dirname $0) && pwd)
cd $DIR/..

PYTHON_VERSION=$1
if [ -z "$PYTHON_VERSION" ]; then
  echo "missing python version" >&2
  exit 1
fi

LIB_VERSION=$(tr -d ' \t\r\n' < VERSION)
PRIMARY_PYMM=$(tr -d ' \t\r\n' < PRIMARY_PYTHON 2>/dev/null || true)
: ${PRIMARY_PYMM:=3.14}

PYMM=$(echo "$PYTHON_VERSION" | cut -d. -f1-2)

if [ "$PYMM" = "$PRIMARY_PYMM" ]; then
  printf "%s\n" "$LIB_VERSION"
else
  printf "%s-py%s\n" "$LIB_VERSION" "$PYTHON_VERSION"
fi
