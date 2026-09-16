#!/usr/bin/env python3
"""Digest-preserving image publication for the personal Nexus registry.

Only candidate-* tags may be overwritten by builders. Final build-* tags are
published by this workflow's single publisher, and retries compare exact bytes.
"""
import argparse, base64, hashlib, json, os, re, sys, urllib.error, urllib.request
from pathlib import Path

REGISTRY='docker.nexus.ixuni.win'
ACCEPT=', '.join(['application/vnd.oci.image.index.v1+json','application/vnd.docker.distribution.manifest.list.v2+json','application/vnd.oci.image.manifest.v1+json','application/vnd.docker.distribution.manifest.v2+json'])
INDEX='application/vnd.oci.image.index.v1+json'
def request(method,path,data=None,media=None,missing=False):
    user=os.environ.get('NEXUS_USERNAME') or os.environ.get('PLUGIN_USERNAME')
    password=os.environ.get('NEXUS_PASSWORD') or os.environ.get('PLUGIN_PASSWORD')
    if not user or not password:raise RuntimeError('Nexus credential environment is missing')
    headers={'Accept':ACCEPT,'Authorization':'Basic '+base64.b64encode((user+':'+password).encode()).decode()}
    if media:headers['Content-Type']=media
    req=urllib.request.Request('https://'+REGISTRY+'/v2/'+path,data=data,headers=headers,method=method)
    try:
        with urllib.request.urlopen(req,timeout=90) as r:return r.read(),r.headers
    except urllib.error.HTTPError as e:
        if missing and e.code==404:return None,None
        raise RuntimeError(f'Registry {method} {path}: HTTP {e.code}') from None
def get(repo,tag,missing=False):return request('GET',repo+'/manifests/'+tag,missing=missing)
def digest(raw):return 'sha256:'+hashlib.sha256(raw).hexdigest()
def descriptors(repo,raw):
    doc=json.loads(raw)
    if 'manifests' in doc:
        result=[]
        for entry in doc['manifests']:
            child,_=get(repo,entry['digest'])
            if digest(child)!=entry['digest']:raise RuntimeError('Child digest mismatch')
            # Preserve child image manifests and attestation descriptors exactly.
            if 'manifests' in json.loads(child):result.extend(descriptors(repo,child))
            else:result.append(entry)
        return result
    cfg,_=request('GET',repo+'/blobs/'+doc['config']['digest'])
    if digest(cfg)!=doc['config']['digest']:raise RuntimeError('Config digest mismatch')
    cfg=json.loads(cfg)
    p={k:cfg[k] for k in ['os','architecture','variant'] if cfg.get(k)}
    return [{'mediaType':doc['mediaType'],'size':len(raw),'digest':digest(raw),'platform':p}]
def check_platforms(repo,raw,expected):
    entries=descriptors(repo,raw)
    found={e.get('platform',{}).get('architecture') for e in entries if e.get('platform',{}).get('os')=='linux'}
    if found!=set(expected):raise RuntimeError(f'{repo}: expected {expected}, found {sorted(found)}')
    return entries
def publish(repo,tag,raw,mutable=False):
    existing,_=get(repo,tag,missing=True)
    if existing is not None and existing!=raw and not mutable:
        raise RuntimeError(f'{repo}:{tag} is immutable; existing digest differs')
    if existing!=raw:request('PUT',repo+'/manifests/'+tag,raw,json.loads(raw)['mediaType'])
    verify,_=get(repo,tag)
    if verify!=raw:raise RuntimeError('Published digest mismatch')
    return digest(raw)
def record(repo,tag,d):
    folder=Path('.ci/digests');folder.mkdir(parents=True,exist_ok=True)
    (folder/repo.replace('/','__')).write_text(d+'\n')
    (folder/(repo.replace('/','__')+'.ref')).write_text(REGISTRY+'/'+repo+':'+tag+'@'+d+'\n')
    print(REGISTRY+'/'+repo+':'+tag+'@'+d)
def promote(repo,source,target,platforms):
    raw,_=get(repo,source);check_platforms(repo,raw,platforms)
    d=publish(repo,target,raw);record(repo,target,d)
def merge(repo,tag,platforms,aliases):
    entries=[]
    for arch in platforms:
        raw,_=get(repo,tag+'-'+arch)
        entries.extend(check_platforms(repo,raw,[arch]))
    # Stable serialization permits byte-for-byte retry verification.
    raw=json.dumps({'schemaVersion':2,'mediaType':INDEX,'manifests':entries},separators=(',',':')).encode()
    check_platforms(repo,raw,platforms)
    d=publish(repo,tag,raw)
    for alias in aliases:publish(repo,alias,raw,mutable=True)
    record(repo,tag,d)
def resolve(repo,tag,aliases):
    raw,_=get(repo,tag);d=digest(raw)
    for alias in aliases:publish(repo,alias,raw,mutable=True)
    record(repo,tag,d)
def pin_files():
    config=Path('.ci/deploy-files.json')
    if not config.exists():return
    refs={}
    for p in Path('.ci/digests').glob('*.ref'):
        ref=p.read_text().strip().split('@',1)[0];refs[ref.split(':',1)[0]]=ref
    for name in json.loads(config.read_text()):
        p=Path(name)
        text=p.read_text()
        for repo,ref in refs.items():
            text=re.sub(re.escape(repo)+r'(?=[:@\s"\'])(?::[A-Za-z0-9_.-]+)?(?:@sha256:[0-9a-f]{64})?',lambda _:ref,text)
        p.write_text(text)
def main():
    parser=argparse.ArgumentParser();sub=parser.add_subparsers(dest='action',required=True)
    p=sub.add_parser('promote');p.add_argument('repo');p.add_argument('source');p.add_argument('target');p.add_argument('platforms',nargs='+')
    p=sub.add_parser('merge');p.add_argument('repo');p.add_argument('tag');p.add_argument('platforms',nargs='+');p.add_argument('--alias',action='append',default=[])
    p=sub.add_parser('resolve');p.add_argument('repo');p.add_argument('tag');p.add_argument('--alias',action='append',default=[])
    sub.add_parser('pin-files');a=parser.parse_args()
    if a.action=='promote':promote(a.repo,a.source,a.target,a.platforms)
    elif a.action=='merge':merge(a.repo,a.tag,a.platforms,a.alias)
    elif a.action=='resolve':resolve(a.repo,a.tag,a.alias)
    else:pin_files()
if __name__=='__main__':
    try:main()
    except Exception as e:print(str(e),file=sys.stderr);sys.exit(1)
