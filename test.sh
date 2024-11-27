#!/bin/bash

set -e

# Configuration
RCLONE_CONFIG="[virt]
type = virtualfs
root_directory = ./test_virtualfs"

TEST_DIR="test_files"
VIRT_REMOTE="virt:"
LOG_FILE="sync.log"
INGESTION_DIR="ingestion_processed"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

#######################################
# Setup Functions
#######################################

setup_environment() {
    echo "Setting up test environment..."
    # Create rclone config
    echo "$RCLONE_CONFIG" > rclone.conf
    export RCLONE_CONFIG="$(pwd)/rclone.conf"

    # Clean up previous test environment
    rm -rf "$TEST_DIR"
    mkdir -p "$TEST_DIR"
    rm -rf test_virtualfs
    rm -rf "$INGESTION_DIR"
    mkdir -p "$INGESTION_DIR"
    # Clean up any previous log files
    rm -f "$LOG_FILE"
    echo "Environment cleaned: $TEST_DIR, test_virtualfs, $INGESTION_DIR"
}

create_test_files() {
    echo "Creating test files..."
    echo "content1" > "$TEST_DIR/file1.txt"
    echo "content2" > "$TEST_DIR/file2.txt"
    mkdir -p "$TEST_DIR/subdir"
    echo "subcontent" > "$TEST_DIR/subdir/file3.txt"
}

run_sync() {
    echo "Running rclone sync..."
    ./rclone sync "$TEST_DIR" "$VIRT_REMOTE" --combined "$LOG_FILE" -v --check-first --inplace -c
}

simulate_ingestion_process() {
    echo "Simulating ingestion process..."
    # Move all files from the backend storage to the ingestion processed directory, excluding .delete files and sqlite db
    find test_virtualfs -type f ! -name '*.delete' ! -name '*.db' -exec mv {} "$INGESTION_DIR" \;
    # The backend storage should now only contain .delete files and sqlite db
}

#######################################
# Log Parsing Functions
#######################################

initialize_log_arrays() {
    # Initialize arrays to hold log entries
    identical_files=()
    added_files=()
    deleted_files=()
    updated_files=()
    error_files=()
}

parse_log_entries() {
    initialize_log_arrays
    while IFS= read -r line; do
        symbol="${line:0:1}"
        filepath="${line:2}"

        case "$symbol" in
            "=")
                identical_files+=("$filepath")
                ;;
            "+")
                added_files+=("$filepath")
                ;;
            "-")
                deleted_files+=("$filepath")
                ;;
            "*")
                updated_files+=("$filepath")
                ;;
            "!")
                error_files+=("$filepath")
                ;;
            *)
                echo -e "${RED}✗ Unknown symbol '$symbol' in log file${NC}"
                exit 1
                ;;
        esac
    done < "$LOG_FILE"
}

#######################################
# Verification Functions
#######################################

verify_no_errors() {
    if [ "${#error_files[@]}" -eq 0 ]; then
        echo -e "${GREEN}✓ No errors reported during sync${NC}"
    else
        echo -e "${RED}✗ Errors occurred during sync:${NC}"
        printf '%s\n' "${error_files[@]}"
        exit 1
    fi
}

verify_files_added() {
    local expected_files=("$@")
    local all_matched=true

    for expected_file in "${expected_files[@]}"; do
        if printf '%s\n' "${added_files[@]}" | grep -qx "$expected_file"; then
            echo -e "${GREEN}✓ File '$expected_file' was added to the destination${NC}"
        else
            echo -e "${RED}✗ File '$expected_file' was not added as expected${NC}"
            all_matched=false
        fi
    done

    if [ "$all_matched" = false ]; then
        echo -e "${RED}✗ Not all expected files were added${NC}"
        exit 1
    fi
}

verify_files_deleted() {
    local expected_files=("$@")
    local all_matched=true

    for expected_file in "${expected_files[@]}"; do
        if printf '%s\n' "${deleted_files[@]}" | grep -qx "$expected_file"; then
            echo -e "${GREEN}✓ File '$expected_file' was deleted from the destination${NC}"
        else
            echo -e "${RED}✗ File '$expected_file' was not deleted as expected${NC}"
            all_matched=false
        fi
    done

    if [ "$all_matched" = false ]; then
        echo -e "${RED}✗ Not all expected files were deleted${NC}"
        exit 1
    fi
}

verify_files_updated() {
    local expected_files=("$@")
    local all_matched=true

    for expected_file in "${expected_files[@]}"; do
        if printf '%s\n' "${updated_files[@]}" | grep -qx "$expected_file"; then
            echo -e "${GREEN}✓ File '$expected_file' was updated${NC}"
        else
            echo -e "${RED}✗ File '$expected_file' was not updated as expected${NC}"
            all_matched=false
        fi
    done

    if [ "$all_matched" = false ]; then
        echo -e "${RED}✗ Not all expected files were updated${NC}"
        exit 1
    fi
}

verify_no_redundant_resync() {
    echo "Verifying no redundant re-sync..."
    run_sync
    parse_log_entries
    if [ "${#added_files[@]}" -eq 0 ] && [ "${#updated_files[@]}" -eq 0 ]; then
        echo -e "${GREEN}✓ No redundant files were re-synced${NC}"
    else
        echo -e "${RED}✗ Redundant files were re-synced${NC}"
        if [ "${#added_files[@]}" -ne 0 ]; then
            echo "Unexpectedly added files:"
            printf '%s\n' "${added_files[@]}"
        fi
        if [ "${#updated_files[@]}" -ne 0 ]; then
            echo "Unexpectedly updated files:"
            printf '%s\n' "${updated_files[@]}"
        fi
        exit 1
    fi
}

verify_delete_file_created() {
    local file="$1"
    if [ -f "test_virtualfs/$file.delete" ]; then
        echo -e "${GREEN}✓ .delete file created for '$file'${NC}"
    else
        echo -e "${RED}✗ .delete file not found for '$file'${NC}"
        exit 1
    fi
}

verify_file_exists_in_backend() {
    local file="$1"
    if [ -f "test_virtualfs/$file" ]; then
        echo -e "${GREEN}✓ File '$file' exists in backend storage${NC}"
    else
        echo -e "${RED}✗ File '$file' does not exist in backend storage${NC}"
        exit 1
    fi
}

verify_file_absent_in_backend() {
    local file="$1"
    if [ ! -f "test_virtualfs/$file" ]; then
        echo -e "${GREEN}✓ File '$file' is absent in backend storage${NC}"
    else
        echo -e "${RED}✗ File '$file' should not exist in backend storage${NC}"
        exit 1
    fi
}

verify_file_metadata() {
    local file="$1"
    local expected_deleted="$2"
    local db="test_virtualfs/virtualfs.db"

    if [ ! -f "$db" ]; then
        echo -e "${RED}✗ Database file not found${NC}"
        exit 1
    fi

    local deleted=$(sqlite3 "$db" "SELECT deleted FROM files WHERE remote='$file';")
    if [ "$deleted" == "$expected_deleted" ]; then
        echo -e "${GREEN}✓ Metadata for '$file' has correct 'deleted' status: $deleted${NC}"
    else
        echo -e "${RED}✗ Metadata for '$file' has incorrect 'deleted' status: $deleted${NC}"
        exit 1
    fi
}

verify_file_hash() {
    local file="$1"
    local source_file="$TEST_DIR/$file"
    local backend_file="test_virtualfs/$file"
    local db="test_virtualfs/virtualfs.db"

    if [ ! -f "$source_file" ]; then
        echo -e "${RED}✗ Source file '$source_file' not found${NC}"
        exit 1
    fi

    if [ ! -f "$backend_file" ]; then
        echo -e "${RED}✗ Backend file '$backend_file' not found${NC}"
        exit 1
    fi

    local source_hash=$(md5sum "$source_file" | awk '{print $1}')
    local backend_hash=$(md5sum "$backend_file" | awk '{print $1}')
    local db_hash=$(sqlite3 "$db" "SELECT hash FROM files WHERE remote='$file';")

    if [ "$source_hash" == "$backend_hash" ] && [ "$backend_hash" == "$db_hash" ]; then
        echo -e "${GREEN}✓ Hash for '$file' matches between source, backend, and database${NC}"
    else
        echo -e "${RED}✗ Hash mismatch for '$file'${NC}"
        echo "Source hash: $source_hash"
        echo "Backend hash: $backend_hash"
        echo "Database hash: $db_hash"
        exit 1
    fi
}

#######################################
# Test Cases
#######################################

test_ingestion_process() {
    echo "=== Testing Ingestion Process ==="
    create_test_files
    run_sync
    parse_log_entries
    verify_files_added "file1.txt" "file2.txt" "subdir/file3.txt"
    verify_no_errors

    # Verify files exist in backend storage
    verify_file_exists_in_backend "file1.txt"
    verify_file_exists_in_backend "file2.txt"
    verify_file_exists_in_backend "subdir/file3.txt"

    # Verify metadata
    verify_file_metadata "file1.txt" "0"
    verify_file_metadata "file2.txt" "0"
    verify_file_metadata "subdir/file3.txt" "0"

    # Simulate ingestion process
    simulate_ingestion_process

    # Verify files are deleted from backend storage
    verify_file_absent_in_backend "file1.txt"
    verify_file_absent_in_backend "file2.txt"
    verify_file_absent_in_backend "subdir/file3.txt"

    # Re-sync without changing source files
    verify_no_redundant_resync
}

test_file_deletion() {
    echo "=== Testing File Deletion Handling ==="
    # Delete a file on the source
    rm "$TEST_DIR/file2.txt"
    run_sync
    parse_log_entries
    verify_files_deleted "file2.txt"
    verify_no_errors

    # Verify that a .delete file was created in the backend storage
    verify_delete_file_created "file2.txt"

    # Verify metadata is marked as deleted
    verify_file_metadata "file2.txt" "1"

    # Simulate ingestion process
    simulate_ingestion_process

    # Verify .delete file still exists (as it signals to the ingestion engine)
    verify_delete_file_created "file2.txt"
}

test_file_change_after_ingestion() {
    echo "=== Testing File Change After Ingestion ==="
    # Modify a file on the source after ingestion
    echo "modified content" > "$TEST_DIR/file1.txt"
    run_sync
    parse_log_entries
    verify_files_updated "file1.txt"
    verify_no_errors

    # Verify updated file exists in backend storage
    verify_file_exists_in_backend "file1.txt"

    # Verify file hash matches
    verify_file_hash "file1.txt"

    # Simulate ingestion process again
    simulate_ingestion_process

    # Verify file is deleted from backend storage
    verify_file_absent_in_backend "file1.txt"

    # Verify that re-running sync does not re-sync the file if it hasn't changed
    verify_no_redundant_resync
}

test_metadata_persistence() {
    echo "=== Testing Metadata Persistence ==="
    # Verify that metadata remains in the database even after ingestion
    local db="test_virtualfs/virtualfs.db"
    local file="file1.txt"

    local count=$(sqlite3 "$db" "SELECT COUNT(*) FROM files WHERE remote='$file';")
    if [ "$count" -eq 1 ]; then
        echo -e "${GREEN}✓ Metadata for '$file' persists in the database${NC}"
    else
        echo -e "${RED}✗ Metadata for '$file' is missing from the database${NC}"
        exit 1
    fi
}

test_large_file_handling() {
    echo "=== Testing Large File Handling ==="
    # Create a large file
    dd if=/dev/urandom of="$TEST_DIR/large_file.bin" bs=1M count=50 status=none
    run_sync
    parse_log_entries
    verify_files_added "large_file.bin"
    verify_no_errors

    # Simulate ingestion process
    simulate_ingestion_process

    # Verify large file is deleted from backend storage
    verify_file_absent_in_backend "large_file.bin"

    # Re-sync without changes
    verify_no_redundant_resync

    # Modify large file
    dd if=/dev/urandom of="$TEST_DIR/large_file.bin" bs=1M count=50 conv=notrunc status=none
    run_sync
    parse_log_entries
    verify_files_updated "large_file.bin"
    verify_no_errors
}

test_delete_directory_handling() {
    echo "=== Testing Delete Directory Handling ==="
    # Delete a directory on the source
    rm -rf "$TEST_DIR/subdir"
    run_sync
    parse_log_entries
    verify_files_deleted "subdir/file3.txt"
    verify_no_errors

    # Verify .delete file is created for the file in the deleted directory
    verify_delete_file_created "subdir/file3.txt"

    # Verify that the directory is handled correctly
    # Depending on the backend implementation, directories may not have .delete files
    # So we check that the file's deletion is correctly processed
}

test_symlink_handling() {
    echo "=== Testing Symlink Handling ==="
    # Create a symlink in the source
    ln -s "file1.txt" "$TEST_DIR/symlink_to_file1.txt"
    run_sync
    parse_log_entries
    # Symlinks are generally not synced unless --copy-links is used
    # Verify that the symlink is not added
    if printf '%s\n' "${added_files[@]}" | grep -qx "symlink_to_file1.txt"; then
        echo -e "${RED}✗ Symlink 'symlink_to_file1.txt' was unexpectedly synced${NC}"
        exit 1
    else
        echo -e "${GREEN}✓ Symlink 'symlink_to_file1.txt' was not synced as expected${NC}"
    fi
}

run_all_tests() {
    setup_environment

    test_ingestion_process
    test_file_deletion
    test_file_change_after_ingestion
    test_metadata_persistence
    test_large_file_handling
    test_delete_directory_handling
    test_symlink_handling

    echo -e "${GREEN}All tests passed successfully!${NC}"
}

# Run all tests
run_all_tests
