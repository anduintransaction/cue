import os
import subprocess
from re import split

def run_command_on_files(relative_folder_path, command):
    # Get the current working directory
    cwd = os.getcwd()

    # Construct the full folder path
    folder_path = os.path.join(cwd, relative_folder_path)

    print(folder_path)

    """
    Run a command for all files in the specified folder.

    :param folder_path: Path to the folder containing the files.
    :param command: The command to run (use `{}` as a placeholder for the file path).
    """
    # Check if the folder exists
    if not os.path.isdir(folder_path):
        print(f"Error: The folder '{folder_path}' does not exist.")
        return

    # Iterate over all files in the folder
    for filename in os.listdir(folder_path):
        file_path = os.path.join(folder_path, filename)

         # Skip directories and non-.cue files
        if os.path.isfile(file_path) and filename.endswith(".cue"):
            new_file_path = file_path.replace(".cue", ".json")
            # Replace `{}` in the command with the file path
            formatted_command = command.format(file_path, new_file_path)

            # Run the command
            print(f"Running command: {formatted_command}")
            try:
                subprocess.run(formatted_command, shell=True, check=True)
            except subprocess.CalledProcessError as e:
                print(f"Error running command for file '{file_path}': {e}")

if __name__ == "__main__":
    # Example usage
    relative_folder_path = "./encoding/openapi/testdata/"  # Replace with your folder path
    command = "./cmd/cue/cue def {} -o {} --out openapi --force"  # Replace with your command (use `{}` as a placeholder for the file path)

    run_command_on_files(relative_folder_path, command)
