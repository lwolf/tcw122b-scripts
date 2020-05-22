package sunrise

import (
	"time"

	"github.com/LopatkinEvgeniy/clock"
	"github.com/kelvins/sunrisesunset"
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
	GetSunriseSunset() (time.Time, time.Time, error)
}

type realSunriser struct {
	cl  clock.Clock
	tz  time.Location
	lat float64
	lon float64
}

func (rs *realSunriser) GetSunriseSunset() (time.Time, time.Time, error) {
	localTime := rs.cl.Now().In(&rs.tz)
	_, offset := localTime.Zone()
	offsetHours := (time.Duration(offset) * time.Second).Hours()
	p := sunrisesunset.Parameters{
		Latitude:  rs.lat,
		Longitude: rs.lon,
		UtcOffset: offsetHours,
		Date:      localTime,
	}
	// Calculate the sunrise and sunset times
	sunrise, sunset, err := p.GetSunriseSunset()

	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return reconstructDate(localTime, sunrise), reconstructDate(localTime, sunset), nil
}

func NewRealSunriser(cl clock.Clock, tz time.Location, lat float64, lon float64) *realSunriser {
	return &realSunriser{
		cl:  cl,
		tz:  tz,
		lat: lat,
		lon: lon,
	}
}
