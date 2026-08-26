package main

import "github.com/sirupsen/logrus"

var log = logrus.NewEntry(logrus.StandardLogger())

// applyLogLevel - without this LOG_LEVEL is parsed and then ignored, so the
// logger stays at Info and every Debug line in the code is dead.
func applyLogLevel(name string) {
	lvl, err := logrus.ParseLevel(name)
	if err != nil {
		log.WithField("log_level", name).Warn("unknown LOG_LEVEL, staying at info")
		return
	}
	logrus.SetLevel(lvl)
}
