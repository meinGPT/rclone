# VirtualFS Backend for Rclone

## Introduction

This project introduces a new backend called `virtualfs` for [rclone](https://rclone.org/), a powerful command-line program used for syncing files and directories to and from various cloud storage providers. The `virtualfs` backend is designed specifically to optimize file synchronization for data ingestion processes. It persists file metadata and temporarily stores file content, ensuring that files are ingested from the remote source only once. This approach prevents re-syncing of processed files, saving both bandwidth and storage space.

## Why We Added a New Backend to Rclone

In data ingestion pipelines and processing workflows, efficiency is paramount. Traditional synchronization methods provided by existing rclone backends are not optimized for scenarios where:

- Files need to be processed only once and should not be re-downloaded if they haven't changed.
- Storage space is at a premium, and file content should not be stored longer than necessary.
- There's a need to track files that have been deleted on the source so that downstream systems can update accordingly.

The `virtualfs` backend addresses these specific needs by:

- **Minimizing Redundant Data Transfers**: By persisting file metadata even after content deletion, it avoids re-downloading files that have already been processed and haven't changed.
- **Optimizing Storage Usage**: It stores file content only temporarily during processing, freeing up space once the ingestion is complete.
- **Providing Deletion Signals**: It creates `.delete` files when files are deleted on the source, signaling to ingestion processes that corresponding records should be removed.

## How It Works

The `virtualfs` backend operates by maintaining a local SQLite database that stores metadata about the files synchronized from the remote source. Here's a step-by-step explanation:

1. **Configuration**:
   - You define a root directory where the backend will store content and metadata.

2. **Initial Synchronization**:
   - When you run `rclone sync` with the `virtualfs` backend, it copies files from the source to the specified root directory.
   - It records metadata (like file size, modification time, and hash) in the SQLite database.
   - The file content is stored temporarily in the root directory.

3. **Ingestion Process**:
   - After synchronization, an external ingestion process (e.g., a data processing script) processes the files from the root directory.
   - Once processed, the ingestion process deletes the files from the root directory to free up space.

4. **Metadata Persistence**:
   - Despite the deletion of file content, the metadata remains in the SQLite database.
   - This ensures that during subsequent synchronizations, the backend recognizes processed files and does not re-sync them unless they've changed on the source.

5. **Handling File Changes**:
   - If a file changes on the source (detected via hash or modification time), the backend will re-sync it.
   - This ensures that the ingestion process receives the updated content.

6. **Handling Deletions**:
   - If a file is deleted on the source, the backend creates a corresponding `.delete` file in the root directory.
   - This `.delete` file signals to the ingestion process that the file should be removed from downstream systems or databases.

7. **Avoiding Redundant Syncs**:
   - Since the backend tracks metadata, files that haven't changed are not re-synced, even if their content has been deleted locally.
   - This saves bandwidth and reduces unnecessary processing.

## Why It's Needed

Traditional backends in rclone are designed for general-purpose file synchronization, which may not be efficient for ingestion workflows due to:

- **Re-downloading Processed Files**: Without persistent metadata, the same files may be re-downloaded and re-processed, wasting resources.
- **Storage Constraints**: Keeping all synchronized files consumes significant storage, especially for large datasets.
- **Lack of Deletion Signals**: There's no built-in mechanism to inform ingestion processes about deletions on the source.

The `virtualfs` backend is needed to:

- **Optimize Ingestion Pipelines**: By preventing unnecessary data transfers and processing, it streamlines ingestion workflows.
- **Conserve Storage**: Temporary storage of file content frees up space for other processes.
- **Maintain Data Integrity**: Persistent metadata ensures accurate tracking of file states across synchronizations.
- **Provide Clear Deletion Handling**: `.delete` files offer a straightforward way to handle deletions in downstream systems.

## How to Run the Tests

To ensure that the `virtualfs` backend functions correctly, especially in conjunction with ingestion processes, a comprehensive test script has been provided. This script covers various scenarios, including initial synchronization, ingestion simulation, file updates, deletions, and metadata verification.

### Prerequisites

- **Rclone with VirtualFS Backend**:
  - Ensure that you have built `rclone` with the `virtualfs` backend included.
  - The `rclone` executable should be accessible in the project directory or adjust the script to point to the correct location.

- **SQLite3**:
  - Install the `sqlite3` command-line tool, as the test script interacts with the SQLite database for verification.

- **Bash Shell**:
  - The test script is written in bash and should be run in a Unix-like environment.

### Steps to Run the Tests

1. **Clone the Repository**

2. **Build Rclone with VirtualFS Backend**:

   - go build

3. **Make the Test Script Executable**:

   The test script is named `test_virtualfs.sh`.

   ```bash
   chmod +x test_virtualfs.sh
   ```

4. **Run the Test Script**:

   ```bash
   ./test_virtualfs.sh
   ```

   - The script will execute multiple tests and output the progress and results.
   - Successful steps are indicated with green checkmarks (`✓`), while failures are marked with red crosses (`✗`).

### Understanding the Test Script

The test script performs the following:

1. **Setup**:
   - Initializes the test environment by creating necessary directories and configurations.
   - Cleans up any previous test artifacts to ensure a fresh start.

2. **Test Cases**:

   - **Testing Ingestion Process**:
     - Synchronizes initial test files.
     - Simulates the ingestion process by deleting ingested files.
     - Verifies that re-syncing does not re-download files unnecessarily.

   - **Testing File Deletion Handling**:
     - Deletes a file on the source.
     - Runs synchronization and checks for the creation of `.delete` files.
     - Ensures that deletions are correctly signaled to the ingestion process.

   - **Testing File Change After Ingestion**:
     - Modifies a file on the source after ingestion.
     - Ensures that the updated file is re-synced and processed.

   - **Testing Metadata Persistence**:
     - Verifies that metadata remains in the database even after file content is deleted.
     - Ensures that the backend avoids re-syncing unmodified files.

   - **Testing Large File Handling**:
     - Checks the synchronization and ingestion of large files.
     - Verifies that changes to large files are correctly detected and synchronized.

   - **Testing Delete Directory Handling**:
     - Deletes an entire directory on the source.
     - Ensures that all files within the directory are handled appropriately.

   - **Testing Symlink Handling**:
     - Tests how the backend handles symbolic links.
     - By default, rclone does not sync symlinks unless specified.

3. **Verification**:
   - Uses functions to check file existence, metadata, hashes, and logs.
   - Provides detailed output for each verification step.

### Customizing and Extending the Tests

- **Adjust Paths and Configurations**:
  - Modify the variables at the beginning of the script if your setup differs.

- **Add New Test Cases**:
  - You can add more functions to test additional scenarios relevant to your use case.

- **Logging and Debugging**:
  - The script outputs detailed messages to help identify any issues.
  - Review the output if a test fails to understand what went wrong.

### Important Notes

- **Data Safety**:
  - The script modifies and deletes files within the test directories.
  - Ensure these directories are not used for storing important data.

- **Backend Behavior**:
  - The tests assume that the `virtualfs` backend is implemented as per the design outlined above.
  - If you modify the backend code, you may need to adjust the tests accordingly.

## Conclusion

The `virtualfs` backend enhances rclone's capabilities for data ingestion workflows by optimizing synchronization, conserving storage, and maintaining accurate file tracking. By following this README, you can understand the purpose of the backend, how it works, why it's needed, and how to test it thoroughly.

If you encounter any issues or have questions, feel free to open an issue in the repository or contribute to the project.

---

**Note**: Replace `https://github.com/yourusername/yourproject.git` with the actual URL of your repository. Ensure that any paths or configurations mentioned match your project's structure.