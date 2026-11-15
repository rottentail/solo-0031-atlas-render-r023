#!/usr/bin/env python3
"""Run bounded production flows against a temporary Atlas Render server."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parents[1]


def sample():
    return {"width": 320, "height": 240, "background": "#F5F1E8", "layers": [
        {"id": "plate", "kind": "rectangle", "width": 220, "height": 150,
         "transform": {"x": 45, "y": 35, "scaleX": 1, "scaleY": 1, "degrees": 5},
         "opacity": 1, "fill": {"kind": "linear", "color": "#145A70", "endColor": "#32A5A0", "from": {"x": 0, "y": 0}, "to": {"x": 220, "y": 150}}},
        {"id": "accent", "kind": "ellipse", "width": 130, "height": 130,
         "transform": {"x": 150, "y": 95, "scaleX": 1, "scaleY": 1, "degrees": 0},
         "opacity": .85, "fill": {"kind": "solid", "color": "#E87E4F"}},
        {"id": "inset", "kind": "group", "transform": {"x": 150, "y": 100, "scaleX": 1, "scaleY": 1, "degrees": -8}, "opacity": .9,
         "clip": {"x": 0, "y": 0, "width": 90, "height": 75}, "children": [
             {"id": "facet", "kind": "polygon", "width": 100, "height": 100,
              "transform": {"scaleX": 1, "scaleY": 1}, "opacity": .8,
              "fill": {"kind": "solid", "color": "#FFF4D5"}, "points": [{"x": 10, "y": 90}, {"x": 50, "y": 0}, {"x": 100, "y": 90}]}]}]}


def tiny_png():
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + bytes([240, 40, 80, 255] * 2) + b'\0' + bytes([40, 80, 240, 128] * 2))) + chunk(b'IEND', b'')


def alt_png():
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + bytes([10, 200, 60, 255] * 2) + b'\0' + bytes([200, 60, 10, 255] * 2))) + chunk(b'IEND', b'')


class Demo:
    def __init__(self, binary, directory):
        self.binary = binary
        self.directory = directory
        self.proc = None
        self.log = open(directory / 'server.log', 'ab')
        self.start()

    def start(self):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        self.url = f'http://127.0.0.1:{port}'
        env = dict(os.environ, ATLAS_ADDR=f'127.0.0.1:{port}', ATLAS_STATE=str(self.directory / 'state.json'))
        self.proc = subprocess.Popen([str(self.binary)], env=env, stdout=self.log, stderr=self.log)
        for _ in range(100):
            try:
                self.call('GET', '/healthz')
                return
            except (urllib.error.URLError, ConnectionError):
                if self.proc.poll() is not None:
                    raise RuntimeError((self.directory / 'server.log').read_text())
                time.sleep(.03)
        raise RuntimeError('server startup deadline exceeded')

    def stop(self, abrupt=False):
        if self.proc and self.proc.poll() is None:
            (self.proc.kill if abrupt else self.proc.terminate)()
            self.proc.wait(timeout=15)

    def call(self, method, path, body=None, status=200, headers=None):
        hdr = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            hdr['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=hdr)
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            if response.status != status:
                raise RuntimeError(f'{method} {path}: expected {status}, received {response.status}: {data[:500]!r}')
            if response.headers.get('Content-Type', '').startswith('application/json'):
                return json.loads(data)
            return data

    def wait(self, job_id, wanted=('succeeded',)):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            job = self.call('GET', '/v1/jobs/' + job_id)
            if job['state'] in wanted:
                return job
            if job['state'] in ('failed', 'cancelled'):
                raise RuntimeError(f'unexpected job outcome: {job}')
            time.sleep(.025)
        raise RuntimeError('render deadline exceeded')

    def submit(self, snapshot, key='render-one', scale=1):
        return self.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': scale}, 202, {'Idempotency-Key': key})


def image_doc(asset_ids):
    layers = []
    for index, asset_id in enumerate(asset_ids):
        layers.append({'id': f'img-{index}', 'kind': 'image', 'width': 40, 'height': 40,
                       'assetId': asset_id, 'opacity': 1,
                       'transform': {'x': 10 + index * 50, 'y': 10, 'scaleX': 1, 'scaleY': 1, 'degrees': 0}})
    return {'width': 320, 'height': 200, 'background': '#202830', 'layers': layers}


def run_import_conflicts(demo):
    """Preview -> confirm package import with same-name asset conflicts and races."""
    tgt_png, pkg_png = tiny_png(), alt_png()
    assert tgt_png != pkg_png

    # Target environment already holds an asset id that the package will reuse,
    # plus a frozen render that depends on the target bytes.
    target_asset = demo.call('PUT', '/v1/assets/ribbon', tgt_png, 201)
    doc = image_doc(['ribbon'])
    demo.call('PUT', '/v1/scenes/emblem', {'name': '目标印记', 'expectedRevision': 1, 'document': doc})
    frozen = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 201)
    frozen_job = demo.wait(demo.submit(frozen, 'frozen-before-import')['id'])
    frozen_png = demo.call('GET', '/v1/artifacts/' + frozen_job['artifactId'])
    frozen_digest = hashlib.sha256(frozen_png).hexdigest()

    # A separate server produces the exchange package carrying different bytes
    # under the same asset id (and a brand-new asset id).
    (demo.directory / 'other').mkdir()
    other = Demo(demo.binary, demo.directory / 'other')
    try:
        other.call('PUT', '/v1/assets/ribbon', pkg_png, 201)
        other.call('PUT', '/v1/assets/extra', tgt_png, 201)
        other.call('PUT', '/v1/scenes/source',
                   {'name': '来源印记', 'expectedRevision': 0, 'document': image_doc(['ribbon', 'extra'])})
        src_snap = other.call('POST', '/v1/scenes/source/snapshots', {'expectedRevision': 1}, 201)
        package = other.call('GET', '/v1/snapshots/' + src_snap['id'] + '/package')
    finally:
        other.stop()

    def body(dest):
        return {'name': '导入的印记', 'package': package}, dest

    # 1) Preview is read-only and human understandable; repeating it changes nothing.
    payload, _ = body('adopted-scene')
    preview = demo.call('POST', '/v1/imports/adopted-scene/preview', payload)
    assert preview['sceneStatus'] == 'free'
    rows = {a['assetId']: a for a in preview['assets']}
    assert rows['ribbon']['status'] == 'contentConflict'
    assert rows['ribbon']['packageDigest'] == hashlib.sha256(pkg_png).hexdigest()
    assert rows['ribbon']['targetDigest'] == target_asset['digest']
    assert set(rows['ribbon']['allowed']) == {'keep', 'adopt'}
    assert rows['extra']['status'] == 'new'
    assert preview['summary'] == {'new': 1, 'identical': 0, 'contentConflict': 1, 'retiredConflict': 0}
    assert preview['fingerprint']
    demo.call('POST', '/v1/imports/adopted-scene/preview', payload)
    demo.call('GET', '/v1/scenes/adopted-scene', status=404)

    # 2) Legacy one-shot import now answers 409 with the preview attached.
    err = demo.call('POST', '/v1/imports/adopted-scene', payload, 409)
    assert err['code'] == 'conflict' and err['preview']['fingerprint'] == preview['fingerprint']
    demo.call('GET', '/v1/scenes/adopted-scene', status=404)

    # 3) A confirm without an explicit decision fails and lands nothing.
    demo.call('POST', '/v1/imports/adopted-scene/confirm',
              {'name': '导入的印记', 'fingerprint': preview['fingerprint'],
               'package': package, 'decisions': {}}, 400)
    demo.call('GET', '/v1/scenes/adopted-scene', status=404)
    demo.call('POST', '/v1/imports/adopted-scene/confirm',
              {'name': '导入的印记', 'fingerprint': preview['fingerprint'],
               'package': package, 'decisions': {'ribbon': 'replace'}}, 400)

    # 4) Adopt: package bytes enter under a fresh id; target asset keeps its id.
    adopted = demo.call('POST', '/v1/imports/adopted-scene/confirm',
                        {'name': '导入的印记', 'fingerprint': preview['fingerprint'],
                         'package': package, 'decisions': {'ribbon': 'adopt'}}, 201)
    new_id = adopted['remappedAssets']['ribbon']
    assert new_id != 'ribbon' and new_id in adopted['addedAssets']
    assert 'extra' in adopted['addedAssets']
    imported_doc = adopted['scene']['document']
    refs = imported_doc['layers'][0]['assetId'], imported_doc['layers'][1]['assetId']
    assert refs == (new_id, 'extra')
    # A re-preview against a fresh destination still sees the original target id
    # holding the target bytes: adopt never overwrote or renamed it.
    re_preview = demo.call('POST', '/v1/imports/another-scene/preview',
                           {'name': '再次导入', 'package': package})
    re_rows = {a['assetId']: a for a in re_preview['assets']}
    assert re_rows['ribbon']['status'] == 'contentConflict'
    assert re_rows['ribbon']['targetDigest'] == target_asset['digest']
    assert new_id not in re_rows  # the package still names ribbon, not the remapped id

    # Frozen render that predates the import is byte-for-byte untouched.
    assert demo.call('GET', '/v1/artifacts/' + frozen_job['artifactId']) == frozen_png
    assert hashlib.sha256(demo.call('GET', '/v1/artifacts/' + frozen_job['artifactId'])).hexdigest() == frozen_digest

    # 5) Asset-axis race: asset changes after preview invalidate the fingerprint.
    race_preview = demo.call('POST', '/v1/imports/race-asset/preview',
                             {'name': '竞争场景', 'package': package})
    demo.call('PUT', '/v1/assets/ribbon', alt_png(), 409)  # same id, new bytes still refused
    demo.call('POST', '/v1/assets/ribbon/retire')
    stale = demo.call('POST', '/v1/imports/race-asset/confirm',
                      {'name': '竞争场景', 'fingerprint': race_preview['fingerprint'],
                       'package': package, 'decisions': {'ribbon': 'adopt'}}, 409)
    assert 'stale' in stale['error']
    demo.call('GET', '/v1/scenes/race-asset', status=404)
    # A refreshed preview turns the row into a retired conflict that forces adopt.
    refreshed = demo.call('POST', '/v1/imports/race-asset/preview',
                          {'name': '竞争场景', 'package': package})
    retired_row = {a['assetId']: a for a in refreshed['assets']}['ribbon']
    assert retired_row['status'] == 'retiredConflict' and retired_row['allowed'] == ['adopt']
    assert retired_row['targetState'] == 'retired'
    forced = demo.call('POST', '/v1/imports/race-asset/confirm',
                       {'name': '竞争场景', 'fingerprint': refreshed['fingerprint'],
                        'package': package, 'decisions': {'ribbon': 'adopt'}}, 201)
    retired_new_id = forced['remappedAssets']['ribbon']
    assert retired_new_id != 'ribbon'
    # The retired target id stays retired; a later preview still reads it that way.
    post_preview = demo.call('POST', '/v1/imports/race-asset-2/preview',
                             {'name': '竞争场景二', 'package': package})
    assert {a['assetId']: a for a in post_preview['assets']}['ribbon']['targetState'] == 'retired'
    assert 'ribbon' not in forced['addedAssets'] and retired_new_id in forced['addedAssets']

    # 6) Scene-axis race: the destination scene appearing after preview blocks the commit.
    scene_preview = demo.call('POST', '/v1/imports/race-scene/preview',
                              {'name': '竞争场景', 'package': package})
    demo.call('PUT', '/v1/scenes/race-scene',
              {'name': '占位', 'expectedRevision': 0, 'document': image_doc(['extra'])})
    err = demo.call('POST', '/v1/imports/race-scene/confirm',
                    {'name': '竞争场景', 'fingerprint': scene_preview['fingerprint'],
                     'package': package, 'decisions': {'ribbon': 'adopt'}}, 409)
    assert 'stale' in err['error']

    # 7) A forged fingerprint is rejected even when nothing else changed.
    demo.call('POST', '/v1/imports/forged/confirm',
              {'name': '伪造', 'fingerprint': 'deadbeef', 'package': package,
               'decisions': {'ribbon': 'adopt'}}, 409)
    demo.call('GET', '/v1/scenes/forged', status=404)

    # 8) Keep: preserve the target asset; the imported scene references its id.
    # Build a conflicting package through a real export so digests are valid.
    demo.call('PUT', '/v1/assets/kept', tgt_png, 201)
    (demo.directory / 'keep-other').mkdir()
    keep_other = Demo(demo.binary, demo.directory / 'keep-other')
    try:
        keep_other.call('PUT', '/v1/assets/kept', pkg_png, 201)
        keep_other.call('PUT', '/v1/scenes/keep-source',
                        {'name': '保留来源', 'expectedRevision': 0, 'document': image_doc(['kept'])})
        keep_snap = keep_other.call('POST', '/v1/scenes/keep-source/snapshots',
                                    {'expectedRevision': 1}, 201)
        keep_pkg = keep_other.call('GET', '/v1/snapshots/' + keep_snap['id'] + '/package')
    finally:
        keep_other.stop()
    keep_preview = demo.call('POST', '/v1/imports/kept-scene/preview',
                             {'name': '保留场景', 'package': keep_pkg})
    assert {a['assetId']: a['status'] for a in keep_preview['assets']}['kept'] == 'contentConflict'
    kept = demo.call('POST', '/v1/imports/kept-scene/confirm',
                     {'name': '保留场景', 'fingerprint': keep_preview['fingerprint'],
                      'package': keep_pkg, 'decisions': {'kept': 'keep'}}, 201)
    assert kept['scene']['document']['layers'][0]['assetId'] == 'kept'
    assert 'kept' in kept['reusedAssets'] and 'kept' not in kept['addedAssets']
    # Keeping the target leaves its bytes untouched; a fresh preview still reads
    # the original target digest under that id.
    keep_again = demo.call('POST', '/v1/imports/kept-scene-2/preview',
                           {'name': '保留场景二', 'package': keep_pkg})
    assert {a['assetId']: a for a in keep_again['assets']}['kept']['targetDigest'] == hashlib.sha256(tgt_png).hexdigest()

    # The pre-existing frozen render remains unchanged after every operation.
    assert hashlib.sha256(demo.call('GET', '/v1/artifacts/' + frozen_job['artifactId'])).hexdigest() == frozen_digest
    return {'workflow': 'import-conflicts', 'adoptedAsset': new_id, 'retiredRemap': retired_new_id,
            'frozenRenderDigest': frozen_digest, 'importedScenes': 4}


def run(demo, workflow, out):
    d = sample()
    put = lambda doc, rev=0: demo.call('PUT', '/v1/scenes/emblem', {'name': '双色印记', 'expectedRevision': rev, 'document': doc})
    scene = put(d)
    if workflow == 'create-scene':
        assert demo.call('GET', '/v1/scenes/emblem')['document'] == scene['document']
        bad = sample(); bad['layers'][0]['transform']['scaleX'] = 0
        demo.call('PUT', '/v1/scenes/invalid', {'name': '无效几何', 'expectedRevision': 0, 'document': bad}, 400)
    elif workflow == 'revise-scene':
        d['background'] = '#F0F0F0'; assert put(d, 1)['revision'] == 2
        demo.call('PUT', '/v1/scenes/emblem', {'name': '旧版本', 'expectedRevision': 1, 'document': d}, 409)
    elif workflow == 'archive-scene':
        archived = demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': 1, 'state': 'archived'})
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': archived['revision']}, 409)
        assert demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': archived['revision'], 'state': 'active'})['revision'] == 3
    if workflow in ('import-asset', 'retire-asset'):
        asset = demo.call('PUT', '/v1/assets/ribbon', tiny_png(), 201)
        assert asset['width'] == 2 and asset['digest'] == hashlib.sha256(tiny_png()).hexdigest()
        demo.call('PUT', '/v1/assets/broken', b'not png', 400)
        layer = {'id': 'ribbon', 'kind': 'image', 'width': 60, 'height': 60, 'assetId': 'ribbon', 'opacity': 1, 'transform': {'x': 20, 'y': 30, 'scaleX': 1, 'scaleY': 1}}
        d['layers'].append(layer); put(d, 1)
        pinned = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 201)
        demo.call('POST', '/v1/assets/ribbon/retire')
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 409)
        result = demo.wait(demo.submit(pinned)['id']); assert result['state'] == 'succeeded'
        return {'workflow': workflow, 'assetDigest': asset['digest'], 'pinnedRender': result['state']}
    current = demo.call('GET', '/v1/scenes/emblem')
    snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision']}, 201)
    if workflow == 'freeze-scene':
        original_digest = snapshot['digest']; d['background'] = '#000000'; put(d, current['revision'])
        assert demo.call('GET', '/v1/snapshots/' + snapshot['id'])['digest'] == original_digest
    if workflow in ('export-package', 'import-package'):
        package = demo.call('GET', '/v1/snapshots/' + snapshot['id'] + '/package')
        restored = demo.call('POST', '/v1/imports/restored', {'name': '恢复的印记', 'package': package}, 201)
        assert restored['document'] == snapshot['document']
        corrupt = json.loads(json.dumps(package)); corrupt['snapshot']['document']['width'] += 1
        demo.call('POST', '/v1/imports/corrupt', {'name': '损坏包', 'package': corrupt}, 400)
        demo.call('GET', '/v1/scenes/corrupt', status=404)
    if workflow == 'import-conflicts':
        result = run_import_conflicts(demo)
        return result
    if workflow in ('cancel-render', 'retry-render', 'recover-render'):
        heavy = sample(); heavy['width'] = 1500; heavy['height'] = 1500
        heavy['layers'] = [dict(heavy['layers'][0], id=f'sheet-{i}', width=1500, height=1500, transform={'scaleX': 1, 'scaleY': 1}) for i in range(16)]
        put(heavy, current['revision'])
        snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
    job = demo.submit(snapshot)
    if workflow == 'submit-render':
        assert demo.submit(snapshot)['id'] == job['id']
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 2}, 409, {'Idempotency-Key': 'render-one'})
    if workflow == 'revoke-snapshot':
        demo.call('POST', '/v1/snapshots/' + snapshot['id'] + '/revoke')
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 1}, 409, {'Idempotency-Key': 'blocked-after-revoke'})
    if workflow in ('cancel-render', 'retry-render'):
        stopped = demo.call('POST', '/v1/jobs/' + job['id'] + '/cancel')
        assert stopped['state'] == 'cancelled'
        if workflow == 'cancel-render':
            time.sleep(.1); assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'cancelled'
            return {'workflow': workflow, 'state': 'cancelled'}
        job = demo.call('POST', '/v1/jobs/' + job['id'] + '/retry', status=202)
        assert job['attempt'] == 2
    if workflow == 'recover-render':
        demo.wait(job['id'], wanted=('running',)); demo.stop(abrupt=True); demo.start()
    result = demo.wait(job['id'])
    png = demo.call('GET', '/v1/artifacts/' + result['artifactId'])
    assert png[:8] == b'\x89PNG\r\n\x1a\n'
    assert struct.unpack('>II', png[16:24]) == (snapshot['document']['width'], snapshot['document']['height'])
    if out:
        Path(out).mkdir(parents=True, exist_ok=True); (Path(out) / 'atlas-sample.png').write_bytes(png)
    if workflow == 'compare-renders':
        other = demo.wait(demo.submit(snapshot, 'render-two')['id'])
        comparison = demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': other['artifactId']})
        assert comparison['changedPixels'] == 0
        d['background'] = '#000000'; put(d, current['revision'])
        altered = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
        changed = demo.wait(demo.submit(altered, 'render-three')['id'])
        assert demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': changed['artifactId']})['changedPixels'] > 0
    if workflow == 'purge-artifact':
        demo.call('DELETE', '/v1/artifacts/' + result['artifactId'])
        demo.call('GET', '/v1/artifacts/' + result['artifactId'], status=410)
        assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'succeeded'
    if workflow == 'observe-service':
        summary = demo.call('GET', '/v1/status'); assert summary['jobs']['succeeded'] >= 1
        events = demo.call('GET', '/v1/events'); assert any(e['kind'] == 'job.succeeded' for e in events)
    return {'workflow': workflow, 'state': result['state'], 'pngBytes': len(png), 'pngDigest': hashlib.sha256(png).hexdigest()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workflow', default='submit-render')
    parser.add_argument('--out')
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='atlas-walkthrough-') as temp:
        directory = Path(temp); binary = directory / 'atlas'
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/atlas'], cwd=ROOT, check=True, timeout=60)
        demo = Demo(binary, directory)
        try:
            print(json.dumps(run(demo, args.workflow, args.out), ensure_ascii=False))
        finally:
            demo.stop(); demo.log.close()


if __name__ == '__main__':
    main()
