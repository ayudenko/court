#!/bin/sh
set -e
# На Fly.io (и вообще при первом монтировании) volume принадлежит root.
# Если стартовали root'ом — выдаём права пользователю court и понижаем привилегии.
if [ "$(id -u)" = "0" ]; then
    chown -R court:court /data
    exec su-exec court "$@"
fi
exec "$@"
