#!/bin/sh

set -e

DIR=$(cd $(dirname $0) && pwd)
cd $DIR/..

PYTHON_STANDALONE_VERSION=$1
PYTHON_VERSION=$2

if [ "$PYTHON_STANDALONE_VERSION" = "" ]; then
  echo "missing python-standalone version"
  exit 1
fi

if [ "$PYTHON_VERSION" = "" ]; then
  echo "missing python version"
  exit 1
fi

if [ ! -z "$(git status --porcelain)" ]; then
  echo "working directory is dirty!"
  exit 1
fi

TAG=$("$DIR/tag-name.sh" "$PYTHON_VERSION")

go run ./python/generate --python-standalone-version=$PYTHON_STANDALONE_VERSION --python-version $PYTHON_VERSION
go run ./pip/generate

echo "tagging as $TAG (python $PYTHON_VERSION, python-build-standalone $PYTHON_STANDALONE_VERSION)"
git checkout --detach
git add -f python/internal/data
git add -f pip/internal/data
git commit -m "python $PYTHON_VERSION + python-build-standalone $PYTHON_STANDALONE_VERSION ($TAG)"
git tag -f $TAG
git checkout -

echo "$TAG" > tag-name
