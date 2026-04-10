#!/bin/bash

# Exit if any command fails
set -e

# Handle Ctrl+C gracefully
cleanup() {
  echo "Stopping all processes..."
  kill 0
}
trap cleanup SIGINT

echo "Starting BCR Client..."
(
  cd ~/Documents/Devlopement/BCR/bcr_client || exit
  GDK_BACKEND=x11 wails dev -tags webkit2_41
) &

CLIENT_PID=$!

echo "Starting BCR Host..."
(
  cd ~/Documents/Devlopement/BCR/bcr_host || exit
  go run .
) &

HOST_PID=$!

# Wait for both processes
wait $CLIENT_PID $HOST_PID