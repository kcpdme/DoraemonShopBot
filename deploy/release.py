"""Run as root on the deployment server with a staged binary; never prints credentials."""
import datetime
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import urllib.request

ENV = Path('/etc/doraemon-shop.env')
BIN = Path('/opt/doraemon-shop-bot/doraemon-bot')
DATA = Path('/var/lib/doraemon-shop/store.json')
SERVICE = 'doraemon-shop'

def request(url, body):
    req = urllib.request.Request(url, json.dumps(body).encode(), {'Content-Type': 'application/json', 'User-Agent': 'DoraemonShop-Deployment/1.0'})
    with urllib.request.urlopen(req, timeout=10) as response:
        return json.load(response)

def main():
    if os.geteuid() != 0 or len(sys.argv) != 2:
        raise RuntimeError('Run as root with the staged binary path')
    staged = Path(sys.argv[1]).resolve(strict=True)
    original = ENV.read_text()
    # Repair only the known malformed address, leaving custom configurations alone.
    updated = original.replace('0xc2132D05D31c914a87C6611C10748AaCb58e8F', '0xc2132D05D31c914a87C6611C10748AEb04B58e8F')
    env = {}
    for line in updated.splitlines():
        if line.strip() and not line.lstrip().startswith('#') and '=' in line:
            key, value = line.split('=', 1)
            env[key.strip()] = value.strip().strip('\"\'')
    for name in ('BEP20_USDT_ADDRESS', 'POLYGON_USDT_ADDRESS', 'BEP20_USDT_CONTRACT', 'POLYGON_USDT_CONTRACT'):
        if not re.fullmatch(r'0x[0-9a-fA-F]{40}', env.get(name, '')):
            raise RuntimeError('Invalid address configuration: ' + name)
    telegram = 'https://api.telegram.org/bot' + env['BOT_TOKEN'] + '/'
    me = request(telegram + 'getMe', {})
    if not me.get('ok'):
        raise RuntimeError('Telegram identity check failed')
    print('Telegram identity verified: @' + me['result']['username'], flush=True)
    channel = env['SALES_CHANNEL']
    if not channel.startswith(('@', '-')):
        channel = '@' + channel
    member = request(telegram + 'getChatMember', {'chat_id': channel, 'user_id': me['result']['id']})
    if not member.get('ok') or member['result'].get('status') not in ('administrator', 'creator') or member['result'].get('can_post_messages') is False:
        raise RuntimeError('Bot needs channel administrator posting permission')
    print('Updates channel permissions verified', flush=True)
    for label, key, chain, decimals, urls in (
        ('BSC', 'BEP20_USDT_CONTRACT', 56, 18, ['https://bsc-rpc.publicnode.com', 'https://1rpc.io/bnb', 'https://bsc.drpc.org']),
        ('Polygon', 'POLYGON_USDT_CONTRACT', 137, 6, ['https://polygon-bor-rpc.publicnode.com', 'https://1rpc.io/matic', 'https://polygon.drpc.org']),
    ):
        verified = False
        for url in urls:
            try:
                c = request(url, {'jsonrpc': '2.0', 'id': 1, 'method': 'eth_chainId', 'params': []})
                d = request(url, {'jsonrpc': '2.0', 'id': 2, 'method': 'eth_call', 'params': [{'to': env[key], 'data': '0x313ce567'}, 'latest']})
                if int(c['result'], 16) == chain and int(d['result'], 16) == decimals:
                    verified = True
                    break

            except Exception as exc:
                print(label + ' provider check: ' + type(exc).__name__ + (' HTTP ' + str(exc.code) if hasattr(exc, 'code') else ''), flush=True)
                continue
        if not verified:
            raise RuntimeError(label + ' chain/token preflight failed; service unchanged')
        print(label + ' chain and token decimals verified', flush=True)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = Path('/var/backups/doraemon-shop') / ('release-' + stamp)
    backup.mkdir(parents=True, mode=0o700)
    os.chmod(backup, 0o700)
    shutil.copy2(BIN, backup / 'doraemon-bot')
    shutil.copy2(ENV, backup / 'environment')
    subprocess.run(['systemctl', 'stop', SERVICE], check=True)
    try:
        shutil.copy2(DATA, backup / 'store.json')
        if updated != original:
            temp = ENV.with_suffix('.release')
            fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'w') as f:
                f.write(updated)
                f.flush()
                os.fsync(f.fileno())
            os.replace(temp, ENV)
        incoming = BIN.with_suffix('.release')
        shutil.copyfile(staged, incoming)
        os.chmod(incoming, 0o755)
        os.replace(incoming, BIN)
        subprocess.run(['systemctl', 'start', SERVICE], check=True)
        time.sleep(3)
        subprocess.run(['systemctl', 'is-active', '--quiet', SERVICE], check=True)
    except Exception:
        subprocess.run(['systemctl', 'stop', SERVICE], check=False)
        shutil.copy2(backup / 'doraemon-bot', BIN)
        shutil.copy2(backup / 'environment', ENV)
        subprocess.run(['systemctl', 'start', SERVICE], check=False)
        raise RuntimeError('Deployment failed; previous binary/config restored, live data retained') from None
    print('Live release active. Backup: ' + str(backup), flush=True)
    if updated != original:
        print('Corrected malformed Polygon token contract', flush=True)

if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        # URL-bearing HTTP exceptions can contain the bot token: never log them.
        print(str(exc) if isinstance(exc, RuntimeError) else 'Release failed: ' + type(exc).__name__, file=sys.stderr)
        sys.exit(1)
