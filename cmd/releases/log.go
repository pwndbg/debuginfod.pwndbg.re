package main

import "github.com/sirupsen/logrus"

var log = logrus.NewEntry(logrus.StandardLogger())

func applyLogLevel(name string) {
	lvl, err := logrus.ParseLevel(name)
	if err != nil {
		log.WithField("log_level", name).Warn("unknown LOG_LEVEL, staying at info")
		return
	}
	logrus.SetLevel(lvl)
}
