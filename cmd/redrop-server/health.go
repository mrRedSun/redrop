package main

import (
	"fmt"
	"net/http"
	"time"
)

func httpGet(url string) error {
	c := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	// The app origin answers /healthz for any non-content Host.
	req.Host = "localhost"
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
