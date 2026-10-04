package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Population must have a dated source; this program cannot infer city size from
// a name. Missing/stale source data must be corrected before running a campaign.
type City struct {
	Name           string
	Population     int
	PopulationYear int
	Source         string
}

const defaultCategories = "кафе,рестораны,салоны красоты,парикмахерские,автосервисы,шиномонтаж,стоматологии,ветеринарные клиники,гостиницы,фитнес-клубы,цветочные магазины,мебельные магазины,ремонт техники,строительные компании,клининг,детские центры,автошколы,ателье,пекарни,юридические услуги"

func loadCities(path string) ([]City, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, e := r.Read()
	if e != nil {
		return nil, e
	}
	if strings.Join(header, ",") != "city,population,population_year,source" {
		return nil, errors.New("CSV header must be city,population,population_year,source")
	}
	cities := []City{}
	seen := map[string]bool{}
	for row := 2; ; row++ {
		record, e := r.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("cities row %d: %w", row, e)
		}
		population, e := strconv.Atoi(record[1])
		if e != nil || population <= 0 {
			return nil, fmt.Errorf("cities row %d: invalid population", row)
		}
		year, e := strconv.Atoi(record[2])
		if e != nil || year < 2020 || year > time.Now().Year() || strings.TrimSpace(record[3]) == "" || strings.TrimSpace(record[0]) == "" {
			return nil, fmt.Errorf("cities row %d: name, population year >=2020 and source required", row)
		}
		if population >= 500000 {
			continue
		}
		name := strings.TrimSpace(record[0])
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		cities = append(cities, City{name, population, year, record[3]})
	}
	if len(cities) == 0 {
		return nil, errors.New("No cities below 500000 in the supplied CSV")
	}
	return cities, nil
}
func campaignQueries(cities []City, categories string) []string {
	queries := []string{}
	for _, c := range cities {
		for _, category := range strings.Split(categories, ",") {
			category = strings.TrimSpace(category)
			if category != "" {
				queries = append(queries, category+" в городе "+c.Name)
			}
		}
	}
	return queries
}
