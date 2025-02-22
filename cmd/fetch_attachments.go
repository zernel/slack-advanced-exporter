package cmd

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var (
	attachmentsApiToken string
	localAttachmentsDir string
)

var fetchAttachmentsCmd = &cobra.Command{
	Use:   "fetch-attachments",
	Short: "Fetch all file attachments and add them to the output archive",
	RunE:  fetchAttachments,
}

func init() {
	fetchAttachmentsCmd.PersistentFlags().StringVar(&attachmentsApiToken, "api-token", "", "Slack API token. Can be obtained here: https://api.slack.com/docs/oauth-test-tokens")
	fetchAttachmentsCmd.PersistentFlags().StringVar(&localAttachmentsDir, "attachments-dir", "", "Local directory containing downloaded attachments")
}

func fetchAttachments(cmd *cobra.Command, args []string) error {
	// Open the input archive.
	r, err := zip.OpenReader(inputArchive)
	if err != nil {
		fmt.Printf("Could not open input archive for reading: %s\n", inputArchive)
		os.Exit(1)
	}
	defer r.Close()

	// Open the output archive.
	f, err := os.Create(outputArchive)
	if err != nil {
		fmt.Printf("Could not open the output archive for writing: %s\n\n%s", outputArchive, err)
		os.Exit(1)
	}
	defer f.Close()

	// Create a zip writer on the output archive.
	w := zip.NewWriter(f)

	// Run through all the files in the input archive.
	for _, file := range r.File {
		verbosePrintln(fmt.Sprintf("Processing file: %s\n", file.Name))

		// Open the file from the input archive.
		inReader, err := file.Open()
		if err != nil {
			fmt.Printf("Failed to open file in input archive: %s\n\n%s", file.Name, err)
			os.Exit(1)
		}

		// Read the file into a byte array.
		inBuf, err := ioutil.ReadAll(inReader)
		if err != nil {
			fmt.Printf("Failed to read file in input archive: %s\n\n%s", file.Name, err)
		}

		// Now write this file to the output archive.
		outFile, err := w.Create(file.Name)
		if err != nil {
			fmt.Printf("Failed to create file in output archive: %s\n\n%s", file.Name, err)
			os.Exit(1)
		}
		_, err = outFile.Write(inBuf)
		if err != nil {
			fmt.Printf("Failed to write file in output archive: %s\n\n%s", file.Name, err)
		}

		// Check if the file name matches the pattern for files we need to parse.
		splits := strings.Split(file.Name, "/")
		if len(splits) == 2 && !strings.HasPrefix(splits[0], "__") && strings.HasSuffix(splits[1], ".json") {
			// Parse this file.
			err = processChannelFile(w, file, inBuf, attachmentsApiToken)
			if err != nil {
				fmt.Printf("%s", err)
				os.Exit(1)
			}
		}
	}

	// Close the output zip writer.
	err = w.Close()
	if err != nil {
		fmt.Printf("Failed to close the output archive.\n\n%s", err)
	}

	return nil
}

func processChannelFile(w *zip.Writer, file *zip.File, inBuf []byte, token string) error {
	verbosePrintln("This is a 'channels' file. Examining it's contents for attachments.")

	// Parse the JSON of the file.
	var posts []SlackPost
	if err := json.Unmarshal(inBuf, &posts); err != nil {
		return errors.New("Couldn't parse the JSON file: " + file.Name + "\n\n" + err.Error() + "\n")
	}

	// Loop through all the posts.
	for _, post := range posts {
		// Support for legacy file_share posts.
		if post.Subtype == "file_share" {
			// Check there's a File property.
			if post.File == nil {
				log.Print("++++++ file_share post has no File property: " + post.Ts + "\n")
				continue
			}

			// Add the file as a single item in the array of the post's files.
			post.Files = []*SlackFile{post.File}
		}

		// If the post doesn't contain any files, move on.
		if post.Files == nil {
			continue
		}

		client := &http.Client{}

		// Loop through all the files.
		for _, file := range post.Files {
			log.Print("\n")
			log.Print("++++++ Processing file: " + file.Id + "\n")
			localFilePath := ""
			if len(file.Id) > 0 && localAttachmentsDir != "" {
				var err error
				localFilePath, err = findLocalAttachment(file.Id, localAttachmentsDir)
				if err == nil {
					if len(file.Name) < 1 {
						fileName := filepath.Base(localFilePath)
						prefix := file.Id + "-"
						if strings.HasPrefix(fileName, prefix) {
							fileName = strings.TrimPrefix(fileName, prefix)
						}
						// Replace spaces and special characters with underscores, but keep the file extension.
						ext := filepath.Ext(fileName)
						fileNameWithoutExt := strings.TrimSuffix(fileName, ext)
						reg := regexp.MustCompile(`[[:space:][:punct:]]`)
						fileNameWithoutExt = reg.ReplaceAllString(fileNameWithoutExt, "_")
						file.Name = fileNameWithoutExt + ext
					}
					log.Print("++++++ Find local attachments: " + file.Id)
				}
			}
			log.Print("++++++ Local file path: " + localFilePath)

			if localFilePath == "" {
				// Check there's an Id, Name and either UrlPrivateDownload or UrlPrivate property.
				if len(file.Id) < 1 || len(file.Name) < 1 || !(len(file.UrlPrivate) > 0 || len(file.UrlPrivateDownload) > 0) {
					log.Print("++++++ file_share post has missing properties on its File object: " + post.Ts + "\n")
					continue
				}
			}

			// Figure out the download URL to use.
			var downloadUrl string
			if len(file.UrlPrivateDownload) > 0 {
				downloadUrl = file.UrlPrivateDownload
			} else {
				downloadUrl = file.UrlPrivate
			}

			// Build the output file path.
			outputPath := "__uploads/" + file.Id + "/" + file.Name

			// Create the file in the zip output file.
			outFile, err := w.Create(outputPath)
			if err != nil {
				log.Print("++++++ Failed to create output file in output archive: " + outputPath + "\n\n" + err.Error() + "\n")
				continue
			}

			verbosePrintln(fmt.Sprintf("Downloading file %s (%s)", file.Id, file.Name))

			// Fetch the file.
			req, err := http.NewRequest("GET", downloadUrl, nil)
			if err != nil {
				log.Print("++++++ Failed to create file download request: " + downloadUrl)
				continue
			}
			if token != "" {
				req.Header.Add("Authorization", "Bearer "+token)
			}
			response, err := client.Do(req)
			if err != nil || response.StatusCode != http.StatusOK {
				// 先尝试本地文件
				if localFilePath, err := findLocalAttachment(file.Id, localAttachmentsDir); err == nil {
					localFile, err := os.Open(localFilePath)
					if err != nil {
						log.Print("++++++ Failed to open the local file: " + localFilePath + "\n\n" + err.Error() + "\n")
						continue
					}
					defer localFile.Close()
					_, err = io.Copy(outFile, localFile)
					if err == nil {
						fmt.Printf("Use local file: %s (%s)\n", file.Id, localFilePath)
						continue
					}
				}

				log.Print("++++++ Download failed and no local attachment.: " + downloadUrl)
				continue
			}
			defer response.Body.Close()

			// Save the file to the output zip file.
			_, err = io.Copy(outFile, response.Body)
			if err != nil {
				log.Print("++++++ Failed to write the downloaded file to the output archive: " + downloadUrl + "\n\n" + err.Error() + "\n")
			}

			// Success at last.
			fmt.Printf("Downloaded attachment into output archive: %s.\n", file.Id)
		}
	}

	return nil
}

func findLocalAttachment(fileID, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("Local attachment directory not configured.")
	}

	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return "", err
	}

	for _, f := range files {
		if strings.HasPrefix(f.Name(), fileID) {
			// file, err := os.Open(filepath.Join(dir, f.Name()))
			// if err != nil {
			// 	return nil, "", err
			// }
			// return file, f.Name(), nil
			filePath := filepath.Join(dir, f.Name())
			return filePath, nil
		}
	}
	return "", errors.New("No matching local attachment found.")
}
