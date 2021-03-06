package sunrise

import (
	"time"
)

// SunriseSunset calculates when the sun will rise and when it will set on the
// given day at the specified location.
func SunriseSunset(latitude, longitude float64, year int, month time.Month, day int) (time.Time, time.Time) {
	var (
		d                 = MeanSolarNoon(longitude, year, month, day)
		solarAnomaly      = SolarMeanAnomaly(d)
		equationOfCenter  = EquationOfCenter(solarAnomaly)
		eclipticLongitude = EclipticLongitude(solarAnomaly, equationOfCenter, d)
		solarTransit      = SolarTransit(d, solarAnomaly, eclipticLongitude)
		declination       = Declination(eclipticLongitude)
		hourAngle         = HourAngle(latitude, declination)
		frac              = hourAngle / 360
		sunrise           = solarTransit - frac
		sunset            = solarTransit + frac
	)
	return JulianDayToTime(sunrise), JulianDayToTime(sunset)
}
