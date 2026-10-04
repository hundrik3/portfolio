package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const populationQuery = `SELECT ?city ?cityLabel ?population ?date WHERE {
 ?city wdt:P17 wd:Q159; wdt:P31/wdt:P279* wd:Q515; p:P1082 ?statement.
 ?statement ps:P1082 ?population; pq:P585 ?date.
 FILTER(?date >= "2020-01-01T00:00:00Z"^^xsd:dateTime)
 FILTER(?population > 0 && ?population < 500000)
 FILTER NOT EXISTS { ?city p:P1082 ?newer. ?newer ps:P1082 ?newPopulation; pq:P585 ?newDate. FILTER(?newDate > ?date) }
 SERVICE wikibase:label { bd:serviceParam wikibase:language "ru". }
}`

func refreshCities(ctx context.Context, path, endpoint string, client *http.Client) error {
	u, e := url.Parse(endpoint)
	if e != nil {
		return e
	}
	query := u.Query()
	query.Set("query", populationQuery)
	query.Set("format", "json")
	u.RawQuery = query.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return e
	}
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", "MapsProspector/0.1 (city population import)")
	response, e := client.Do(req)
	if e != nil {
		return errors.New("Не удалось получить список городов из Wikidata; проверьте сеть")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("Источник городов вернул HTTP %d", response.StatusCode)
	}
	var data struct {
		Results struct {
			Bindings []map[string]struct {
				Value string `json:"value"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if e = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&data); e != nil {
		return errors.New("Некорректные данные населения")
	}
	cities := []City{}
	seen := map[string]bool{}
	for _, row := range data.Results.Bindings {
		name := row["cityLabel"].Value
		population, e := strconv.Atoi(row["population"].Value)
		if e != nil || population <= 0 || population >= 500000 {
			continue
		}
		date, e := time.Parse(time.RFC3339, row["date"].Value)
		if e != nil || date.Year() < 2020 || date.After(time.Now()) {
			continue
		}
		source := row["city"].Value
		if name == "" || source == "" || seen[name] {
			continue
		}
		seen[name] = true
		cities = append(cities, City{name, population, date.Year(), source})
	}
	if len(cities) == 0 {
		return errors.New("Источник не вернул подходящих городов; текущий файл не изменён")
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".cities-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	writer := csv.NewWriter(f)
	writer.Write([]string{"city", "population", "population_year", "source"})
	for _, c := range cities {
		writer.Write([]string{c.Name, strconv.Itoa(c.Population), strconv.Itoa(c.PopulationYear), c.Source})
	}
	writer.Flush()
	if e = writer.Error(); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
