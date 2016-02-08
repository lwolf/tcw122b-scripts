# TCW122B-CM
import datetime
import time

import asyncio
from pysnmp.entity import config
from pysnmp.entity.engine import SnmpEngine
from pysnmp.carrier.asyncio.dgram import udp
from pysnmp.entity.rfc3413.ntfrcv import NotificationReceiver
from pysnmp.entity.rfc3413.oneliner import cmdgen
from pysnmp.proto import rfc1902

NAMESPACE = '1.3.6.1.4.1.38783'
WRITE_CONFIRM = '1.3.6.1.4.1.38783.3.13.0'

# PIR SETTINGS
MANUAL = 0
PIR1 = 4  # DIGITALINPUT1
PIR2 = 8  # DIGITALINPUT2


DEVICE_HOST = '172.44.0.200'
PIR_TIMEOUT = '1.3.6.1.4.1.38783.2.9.3.0'  # RELAY_PULSE_VALUE

R2_WORK_FROM = '1.3.6.1.4.1.38783.2.5.1.1.0'  # TEMP1_MIN_VALUE
R2_WORK_TO = '1.3.6.1.4.1.38783.2.5.1.2.0'  # TEMP1_MAX_VALUE

R1_WORK_FROM = '1.3.6.1.4.1.38783.2.5.1.3.0'  # HUMIDITY_MIN_VALUE
R1_WORK_TO = '1.3.6.1.4.1.38783.2.5.1.4.0'  # HUMIDITY_MAX_VALUE

PIR1_MODE = '1.3.6.1.4.1.38783.2.9.1.0'
PIR2_MODE = '1.3.6.1.4.1.38783.2.9.2.0'

RELAY1_ID = '1.3.6.1.4.1.38783.3.3.0'
RELAY2_ID = '1.3.6.1.4.1.38783.3.5.0'


PIR1_STATE = '1.3.6.1.4.1.38783.3.1.0'
PIR2_STATE = '1.3.6.1.4.1.38783.3.2.0'
CONFIGURATION_SAVED = '1.3.6.1.4.1.38783.3.13.0'

DEBUG_VALUES = {
    '1.3.6.1.4.1.38783.3.1.0': 'digitalInput1State',
    '1.3.6.1.4.1.38783.3.2.0': 'digitalInput2State',
    '1.3.6.1.4.1.38783.3.3.0': 'relay1State',
    '1.3.6.1.4.1.38783.3.4.0': 'relay1Pulse',
    '1.3.6.1.4.1.38783.3.5.0': 'relay2State',
    '1.3.6.1.4.1.38783.3.6.0': 'relay2Pulse',
    '1.3.6.1.4.1.38783.3.7.0': 'voltage1x10Int',
    '1.3.6.1.4.1.38783.3.8.0': 'voltage2x10Int',
    '1.3.6.1.4.1.38783.3.9.0': 'temp1x10Int',
    '1.3.6.1.4.1.38783.3.10.0': 'temp2x10Int',
    '1.3.6.1.4.1.38783.3.11.0': 'humi1x10Int',
    '1.3.6.1.4.1.38783.3.12.0': 'humi2x10Int',
    '1.3.6.1.4.1.38783.3.13.0': 'configurationSaved',
    '1.3.6.1.4.1.38783.3.14.0': 'restartDevice',
}

SETTINGS = {
    RELAY1_ID: {'from': 0, 'to': 0},
    RELAY2_ID: {'from': 0, 'to': 0},
    'timeout': 20,
}

PIR_STATES = {
    PIR1_STATE: {
        'name': 'PIR_1',
        'state': 0,
        'ts': 0,
        'relay': 'r1',
        'relay_id': RELAY1_ID,
        'manual_mode': False,
    },
    PIR2_STATE: {
        'name': 'PIR_2',
        'state': 0,
        'ts': 0,
        'relay': 'r2',
        'relay_id': RELAY2_ID,
        'manual_mode': False,
    },
}


def write_state(list_of_uid_and_values):
    # to every write we should also send WRITE_CONFIRM
    list_of_uid_and_values.append((WRITE_CONFIRM, rfc1902.Integer(1)))
    cmd_gen = cmdgen.CommandGenerator()
    error_indication, error_status, error_index, binds = cmd_gen.getCmd(
        cmdgen.CommunityData('private'),
        cmdgen.UdpTransportTarget((DEVICE_HOST, 161)),
        *list_of_uid_and_values
    )
    # Check for errors and print out results
    if error_indication:
        print(error_indication)
        return
    else:
        if error_status:
            print('%s at %s' % (
                error_status.prettyPrint(),
                error_index and binds[int(error_index) - 1] or '?'
                )
            )
        else:
            for name, val in binds:
                print('%s = %s' % (name.prettyPrint(), val.prettyPrint()))


def read_state(uids):
    cmd_gen = cmdgen.CommandGenerator()
    error_indication, error_status, error_index, binds = cmd_gen.getCmd(
        cmdgen.CommunityData('public'),
        cmdgen.UdpTransportTarget((DEVICE_HOST, 161)),
        *uids
    )

    # Check for errors and print out results
    if error_indication:
        print(error_indication)
        return
    else:
        if error_status:
            print('%s at %s' % (
                error_status.prettyPrint(),
                error_index and binds[int(error_index) - 1] or '?'
                )
            )
        else:
            return {str(n): str(v) for n, v in binds}


def update_config():
    print("get initial configuration")
    results = read_state([PIR1_MODE, PIR2_MODE])

    PIR_STATES[PIR1_STATE]['manual_mode'] = int(results[PIR1_MODE]) == MANUAL
    PIR_STATES[PIR2_STATE]['manual_mode'] = int(results[PIR2_MODE]) == MANUAL


def config_updater(loop):
    loop.call_soon(update_config)
    loop.call_soon(check_schedule)
    # resync config every 15 minutes
    loop.call_later(60 * 15, config_updater, loop)


def check_schedule():
    print("check schedule")
    results = read_state([
        PIR_TIMEOUT, R1_WORK_FROM, R1_WORK_TO, R2_WORK_FROM, R2_WORK_TO]
    )

    SETTINGS['timeout'] = int(results[PIR_TIMEOUT])
    SETTINGS[RELAY1_ID]['from'] = int(results[R1_WORK_FROM]) / 10
    SETTINGS[RELAY1_ID]['to'] = int(results[R1_WORK_TO]) / 10

    SETTINGS[RELAY2_ID]['from'] = int(results[R2_WORK_FROM]) / 10
    SETTINGS[RELAY2_ID]['to'] = int(results[R2_WORK_TO]) / 10


def lights_switcher(loop):
    now = time.time()
    for name, state in PIR_STATES.items():
        if state['state'] == 1 and now - state['ts'] > SETTINGS['timeout']:
            state['state'] = 0
            print("%s: WE SHOULD TURN %s OFF now" % (time.time(), state['relay']))
            write_state([(state['relay_id'], rfc1902.Integer(0))])
    # print("nothing happened", time.time())

    loop.call_later(1, lights_switcher, loop)


def is_valid_timeframe(pir):
    current_hour = datetime.datetime.now().hour
    schedule = SETTINGS[pir['relay_id']]
    if not schedule['from'] and not schedule['to']:
        return True

    if schedule['from'] <= current_hour < schedule['to']:
        return False

    return True


def callback(engine, state_reference, ctx_engine_id, ctx_name, binds, cbCtx):
    """
    Callback function for receiving notifications

    :param engine:
    :param state_reference:
    :param ctx_engine_id:
    :param ctx_name:
    :param binds:
    :param cbCtx:
    :return:
    """
    name, val = binds[-1]
    str_name = str(name)
    if str_name in PIR_STATES:
        content = PIR_STATES[str_name]
        if content['manual_mode'] and int(val) == 1 and is_valid_timeframe(content):
            print("%s: %s should be %s now" % (time.time(), content['relay'], val))
            if content['state'] != 1:
                write_state([(content['relay_id'], rfc1902.Integer(1))])
            content['state'] = 1
            content['ts'] = time.time()

    elif str_name == CONFIGURATION_SAVED:
        print("Looks like config has been changed...updating.")
        update_config()


def configure_engine(engine, host='127.0.0.1', port=162):
    # UDP over IPv4, first listening interface/port
    config.addTransport(
        engine,
        udp.domainName + (1,),
        udp.UdpTransport().openServerMode((host, port))
    )

    config.addV1System(engine, 'my-area', 'public')


if __name__ == '__main__':
    loop = asyncio.get_event_loop()
    engine = SnmpEngine()

    configure_engine(engine, '172.44.0.100')
    receiver = NotificationReceiver(engine, callback)

    loop.call_soon(config_updater, loop)
    loop.call_soon(lights_switcher, loop)

    try:
        loop.run_forever()
    except KeyboardInterrupt:
        print("Turning off..")
        loop.close()
        receiver.close(engine)
