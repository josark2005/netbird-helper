package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	geoLiteCityTarGZURL     = "https://pkgs.netbird.io/geolocation-dbs/GeoLite2-City/download?suffix=tar.gz"
	geoLiteCityZipURL       = "https://pkgs.netbird.io/geolocation-dbs/GeoLite2-City-CSV/download?suffix=zip"
	geoLiteCitySha256TarURL = "https://pkgs.netbird.io/geolocation-dbs/GeoLite2-City/download?suffix=tar.gz.sha256"
	geoLiteCitySha256ZipURL = "https://pkgs.netbird.io/geolocation-dbs/GeoLite2-City-CSV/download?suffix=zip.sha256"
	geoLiteCityMMDB         = "GeoLite2-City.mmdb"
	geoLiteCityCSV          = "GeoLite2-City-Locations-en.csv"
)

type GeoNames struct {
	GeoNameID           int    `gorm:"column:geoname_id"`
	LocaleCode          string `gorm:"column:locale_code"`
	ContinentCode       string `gorm:"column:continent_code"`
	ContinentName       string `gorm:"column:continent_name"`
	CountryIsoCode      string `gorm:"column:country_iso_code"`
	CountryName         string `gorm:"column:country_name"`
	Subdivision1IsoCode string `gorm:"column:subdivision_1_iso_code"`
	Subdivision1Name    string `gorm:"column:subdivision_1_name"`
	Subdivision2IsoCode string `gorm:"column:subdivision_2_iso_code"`
	Subdivision2Name    string `gorm:"column:subdivision_2_name"`
	CityName            string `gorm:"column:city_name"`
	MetroCode           string `gorm:"column:metro_code"`
	TimeZone            string `gorm:"column:time_zone"`
	IsInEuropeanUnion   string `gorm:"column:is_in_european_union"`
}

func (GeoNames) TableName() string {
	return "geonames"
}

func main() {
	outDir := flag.String("o", "", "output directory for database files (required)")
	force := flag.Bool("force", false, "force re-download even if files exist")
	flag.Parse()

	if *outDir == "" {
		fmt.Fprintf(os.Stderr, "Error: -o flag is required\n")
		flag.Usage()
		os.Exit(1)
	}

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory %s: %v", *outDir, err)
	}

	log.Infof("Fetching remote filenames from CDN...")

	mmdbFilename, err := resolveDatabaseFilename(geoLiteCityTarGZURL, "GeoLite2-City_*.mmdb")
	if err != nil {
		log.Fatalf("Failed to resolve MMDB filename: %v", err)
	}

	geonamesFilename, err := resolveDatabaseFilename(geoLiteCityZipURL, "geonames_*.db")
	if err != nil {
		log.Fatalf("Failed to resolve geonames filename: %v", err)
	}

	fmt.Printf("Target MMDB file:      %s\n", mmdbFilename)
	fmt.Printf("Target geonames file:  %s\n", geonamesFilename)

	if *force {
		log.Info("Force mode enabled, will re-download even if files exist")
	}

	if err := loadDatabases(*outDir, mmdbFilename, geonamesFilename, *force); err != nil {
		log.Fatalf("Failed to load databases: %v", err)
	}

	log.Infof("All done. Files saved to %s/", *outDir)
}

func resolveDatabaseFilename(dataURL, pattern string) (string, error) {
	filename, err := getFilenameFromURL(dataURL)
	if err != nil {
		return "", fmt.Errorf("failed to get filename from %s: %w", dataURL, err)
	}
	basename := strings.SplitN(filename, ".", 2)[0]
	parts := strings.SplitN(basename, "_", 2)
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected filename format: %s", filename)
	}
	date := parts[1]
	return strings.Replace(pattern, "*", date, 1), nil
}

func loadDatabases(dataDir, mmdbFile, geonamesDbFile string, force bool) error {
	for _, file := range []struct {
		name         string
		checksumURL  string
		dataURL      string
		extractFunc  func(src, dst string) error
	}{
		{
			name:        mmdbFile,
			checksumURL: geoLiteCitySha256TarURL,
			dataURL:     geoLiteCityTarGZURL,
			extractFunc: func(src, dst string) error {
				if err := decompressTarGzFile(src, dst); err != nil {
					return err
				}
				return copyFile(
					filepath.Join(dst, geoLiteCityMMDB),
					filepath.Join(dataDir, mmdbFile),
				)
			},
		},
		{
			name:        geonamesDbFile,
			checksumURL: geoLiteCitySha256ZipURL,
			dataURL:     geoLiteCityZipURL,
			extractFunc: func(src, dst string) error {
				if err := decompressZipFile(src, dst); err != nil {
					return err
				}
				extractedCsvFile := filepath.Join(dst, geoLiteCityCSV)
				return importCsvToSqlite(dataDir, extractedCsvFile, geonamesDbFile)
			},
		},
	} {
		dstPath := filepath.Join(dataDir, file.name)
		exists := fileExists(dstPath) == nil
		if exists && !force {
			log.Infof("Skipping %s (already exists, use --force to re-download)", file.name)
			continue
		}

		log.Infof("Downloading and processing %s...", file.name)
		if err := loadDatabase(file.checksumURL, file.dataURL, file.extractFunc); err != nil {
			return fmt.Errorf("failed to process %s: %w", file.name, err)
		}
		log.Infof("Successfully created %s", file.name)
	}
	return nil
}

func loadDatabase(checksumURL, fileURL string, extractFunc func(src, dst string) error) error {
	tmpDir, err := os.MkdirTemp("", "netbird-geolite")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	checksumFilename, err := getFilenameFromURL(checksumURL)
	if err != nil {
		return err
	}
	checksumFile := filepath.Join(tmpDir, checksumFilename)
	if err := downloadFile(checksumURL, checksumFile); err != nil {
		return err
	}

	sha256sum, err := loadChecksumFromFile(checksumFile)
	if err != nil {
		return err
	}

	dbFilename, err := getFilenameFromURL(fileURL)
	if err != nil {
		return err
	}
	dbFile := filepath.Join(tmpDir, dbFilename)
	if err := downloadFile(fileURL, dbFile); err != nil {
		return err
	}

	if err := verifyChecksum(dbFile, sha256sum); err != nil {
		return err
	}

	return extractFunc(dbFile, tmpDir)
}

func importCsvToSqlite(dataDir, csvFile, geonamesDbFile string) error {
	geonames, err := loadGeonamesCsv(csvFile)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(dataDir, geonamesDbFile)
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger:          logger.Default.LogMode(logger.Silent),
		CreateBatchSize: 1000,
	})
	if err != nil {
		return err
	}
	defer func() {
		sqlDB, err := db.DB()
		if err != nil {
			return
		}
		sqlDB.Close()
	}()

	if err := db.AutoMigrate(&GeoNames{}); err != nil {
		return err
	}
	return db.Create(geonames).Error
}

func loadGeonamesCsv(csvPath string) ([]GeoNames, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	var geoNames []GeoNames
	for index, record := range records {
		if index == 0 {
			continue
		}
		geoNameID, err := strconv.Atoi(record[0])
		if err != nil {
			return nil, fmt.Errorf("failed to parse geoname_id at line %d: %w", index+1, err)
		}
		geoNames = append(geoNames, GeoNames{
			GeoNameID:           geoNameID,
			LocaleCode:          record[1],
			ContinentCode:       record[2],
			ContinentName:       record[3],
			CountryIsoCode:      record[4],
			CountryName:         record[5],
			Subdivision1IsoCode: record[6],
			Subdivision1Name:    record[7],
			Subdivision2IsoCode: record[8],
			Subdivision2Name:    record[9],
			CityName:            record[10],
			MetroCode:           record[11],
			TimeZone:            record[12],
			IsInEuropeanUnion:   record[13],
		})
	}
	return geoNames, nil
}

func decompressTarGzFile(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzipReader, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if header.Typeflag == tar.TypeReg {
			outFile, err := os.Create(filepath.Join(destDir, filepath.Base(header.Name)))
			if err != nil {
				return err
			}
			_, err = io.Copy(outFile, tarReader)
			outFile.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func decompressZipFile(archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		outFile, err := os.Create(filepath.Join(destDir, filepath.Base(f.Name)))
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func downloadFile(url, dstPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, strings.NewReader(string(bodyBytes)))
	return err
}

func getFilenameFromURL(url string) (string, error) {
	resp, err := http.Head(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if len(resp.Header["Content-Disposition"]) == 0 {
		return "", fmt.Errorf("no Content-Disposition header in response from %s", url)
	}

	_, params, err := mime.ParseMediaType(resp.Header["Content-Disposition"][0])
	if err != nil {
		return "", err
	}

	filename := params["filename"]
	if filename == "" {
		return "", fmt.Errorf("empty filename in Content-Disposition from %s", url)
	}
	return filename, nil
}

func calculateFileSHA256(fPath string) ([]byte, error) {
	f, err := os.Open(fPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func loadChecksumFromFile(fPath string) (string, error) {
	f, err := os.Open(fPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) > 0 {
			return parts[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksum file %s is empty", fPath)
}

func verifyChecksum(fPath, expectedChecksum string) error {
	calculatedChecksum, err := calculateFileSHA256(fPath)
	if err != nil {
		return err
	}
	fileCheckSum := fmt.Sprintf("%x", calculatedChecksum)
	if fileCheckSum != expectedChecksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, fileCheckSum)
	}
	return nil
}

func fileExists(filePath string) error {
	_, err := os.Stat(filePath)
	return err
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}
