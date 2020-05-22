package sunrise

import (
	"time"

	"github.com/LopatkinEvgeniy/clock"
	gs "github.com/nathan-osman/go-sunrise"
)
// reconstructDate takes source date and sunset or sunrise time as target and returns
// new date using year,month,day and location from the source and time from the target.
func reconstructDate(sourceDate time.Time, targetTime time.Time) time.Time {
	return time.Date(
		sourceDate.Year(),
		sourceDate.Month(),
		sourceDate.Day(),
		targetTime.Hour(),
		targetTime.Minute(),
		targetTime.Second(),
		targetTime.Nanosecond(),
		sourceDate.Location(),
	)
}

type Sunriser interface {
	GetSunriseSunset() (time.Time, time.Time)
}

type realSunriser struct {
	cl  clock.Clock
	tz  *time.Location
	lat float64
	lon float64
}

func (rs *realSunriser) GetSunriseSunset() (time.Time, time.Time) {
	localTime := rs.cl.Now().In(rs.tz)
	rise, set := gs.SunriseSunset(
		rs.lat, rs.lon,
		localTime.Year(), localTime.Month(), localTime.Day(),
	)
	return rise.In(rs.tz), set.In(rs.tz)
}

func NewRealSunriser(cl clock.Clock, tz *time.Location, lat float64, lon float64) *realSunriser {
	return &realSunriser{
		cl:  cl,
		tz:  tz,
		lat: lat,
		lon: lon,
	}
}
