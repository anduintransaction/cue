// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package file

// Read reads the contents of a file.
Read: {
	$id: _id
	_id: "tool/file.Read"

	// filename names the file to read.
	//
	// Relative names are taken relative to the current working directory.
	// Slashes are converted to the native OS path separator.
	filename: !=""

	// contents is the read contents. If the contents are constraint to bytes
	// (the default), the file is read as is. If it is constraint to a string,
	// the contents are checked to be valid UTF-8.
	contents: *bytes | string
}

// Append writes contents to the given file.
Append: {
	$id: _id
	_id: "tool/file.Append"

	// filename names the file to append.
	//
	// Relative names are taken relative to the current working directory.
	// Slashes are converted to the native OS path separator.
	filename: !=""

	// permissions defines the permissions to use if the file does not yet exist.
	permissions: int | *0o666

	// contents specifies the bytes to be written.
	contents: bytes | string
}

// Create writes contents to the given file.
Create: {
	$id: _id
	_id: "tool/file.Create"

	// filename names the file to write.
	//
	// Relative names are taken relative to the current working directory.
	// Slashes are converted to the native OS path separator.
	filename: !=""

	// permissions defines the permissions to use if the file does not yet exist.
	permissions: int | *0o666

	// contents specifies the bytes to be written.
	contents: bytes | string
}

// Glob returns a list of files.
Glob: {
	$id: _id
	_id: "tool/file.Glob"

	// glob specifies the pattern to match files with.
	//
	// A relative pattern is taken relative to the current working directory.
	// Slashes are converted to the native OS path separator.
	glob: !=""
	files: [...string]
}

// Mkdir creates a directory at the specified path.
Mkdir: {
	$id: _id
	_id: "tool/file.Mkdir"

	// The directory path to create.
	// If path is already a directory, Mkdir does nothing.
	// If path already exists and is not a directory, Mkdir will return an error.
	path: string

	// When true any necessary parents are created as well.
	createParents: bool | *false

	// Directory mode and permission bits (before umask).
	permissions: int | *0o777
}

// MkdirAll creates a directory at the specified path along with any necessary
// parents.
// If path is already a directory, MkdirAll does nothing.
// If path already exists and is not a directory, MkdirAll will return an error.
MkdirAll: Mkdir & {
	createParents: true
}

// MkdirTemp creates a new temporary directory in the directory dir and sets
// the pathname of the new directory in path.
// It is the caller's responsibility to remove the directory when it is no
// longer needed.
MkdirTemp: {
	$id: _id
	_id: "tool/file.MkdirTemp"

	// The temporary directory is created in the directory specified by dir.
	// If dir is the empty string, MkdirTemp uses the default directory for
	// temporary files.
	dir: string | *""

	// The directory name is generated by adding a random string to the end of pattern.
	// If pattern includes a "*", the random string replaces the last "*" instead.
	pattern: string | *""

	// The absolute path of the created directory.
	path: string
}

// RemoveAll removes path and any children it contains.
// It removes everything it can but returns the first error it encounters.
RemoveAll: {
	$id: _id
	_id: "tool/file.RemoveAll"

	// The path to remove.
	// If the path does not exist, RemoveAll does nothing.
	path: string

	// success contains the status of the removal.
	// If path was removed success is set to true.
	// If path didn't exists success is set to false.
	success: bool
}
