#!/usr/bin/env python3
"""Opt-in Docker integration: build ludock:test first, then run this script.
Creates and removes only unique Ludock fixture containers and their volumes.
Override LUDOCK_TEST_IMAGE to test another locally built runtime image.
"""
import json, os, pathlib, subprocess, tempfile, time, urllib.request, uuid
project='ludock-v2-smoke-'+uuid.uuid4().hex[:8]
root=pathlib.Path(tempfile.mkdtemp(prefix=project+'-',dir=os.environ.get('LUDOCK_TEST_DIRECTORY'))).resolve()
app=project+'-app'
token='test-only-'+uuid.uuid4().hex
compose=root/'compose.yaml'
compose.write_text('''services:
  game:
    image: alpine:3.23
    command: ["sleep", "infinity"]
    environment:
      LITERAL: "cash$$value"
      BRACED: "$${literal}"
    labels:
      ludock.enable: "true"
      ludock.name: "V2 Compose Smoke"
    depends_on: [dependency]
  dependency:
    image: alpine:3.23
    command: ["sleep", "infinity"]
''')
def docker(*args):
    p=subprocess.run(['docker',*args],text=True,capture_output=True)
    if p.returncode: raise RuntimeError('Docker command failed: '+p.stderr[-1000:])
    return p.stdout.strip()
def cli(*args): return docker('compose','--project-name',project,'--project-directory',str(root),'-f',str(compose),*args)
def inspect(cid): return json.loads(docker('inspect',cid))[0]
def request(path,body=None,method=None):
    data=json.dumps(body).encode() if body is not None else None
    req=urllib.request.Request(base+'/api/v1'+path,data=data,headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'},method=method)
    try:
        with urllib.request.urlopen(req,timeout=30) as r: return json.load(r)
    except urllib.error.HTTPError as e: raise RuntimeError(str(e.code)+' '+e.read().decode())
def update(force):
    op=request('/servers/'+sid+'/updates',{'createBackup':False,'skipBackupConfirmation':'V2 Compose Smoke','forceRecreate':force})['operation']
    end=time.time()+300
    while time.time()<end:
        op=request('/operations/'+op['id'])['operation']
        if op['status'] not in ['queued','running']: return op
        time.sleep(.5)
    raise RuntimeError('Operation timeout')
try:
    cli('up','-d')
    before=cli('ps','-q','game'); dep=cli('ps','-q','dependency')
    docker('run','-d','--name',app,'--label','ludock.enable=false','-p','127.0.0.1::3000','-e','LUDOCK_API_TOKEN='+token,'-e','LUDOCK_COMPOSE_ROOTS='+str(root),'-e','UNRELATED_LUDOCK_SECRET=must-not-be-inherited','-v','/var/run/docker.sock:/var/run/docker.sock','-v',str(root)+':'+str(root)+':ro',os.environ.get('LUDOCK_TEST_IMAGE','ludock:test'))
    port=inspect(app)['NetworkSettings']['Ports']['3000/tcp'][0]['HostPort'];base='http://127.0.0.1:'+port
    for _ in range(100):
        try: request('/health');break
        except Exception: time.sleep(.2)
    sid=next(s['id'] for s in request('/servers')['servers'] if s['displayName']=='V2 Compose Smoke')
    request('/compose-projects',{'projectName':project,'projectDirectory':str(root),'composeFiles':['compose.yaml'],'envFiles':[]})
    cap=request('/servers/'+sid+'/update-capability')['capability'];assert cap['available'],cap
    unchanged=update(False);assert unchanged['status']=='already_current',unchanged
    assert cli('ps','-q','game')==before
    forced=update(True);assert forced['status']=='succeeded',forced
    after=cli('ps','-q','game');assert after!=before
    assert cli('ps','-q','dependency')==dep
    info=inspect(after);assert 'LITERAL=cash$value' in info['Config']['Env'],info['Config']['Env']
    assert 'BRACED=${literal}' in info['Config']['Env'],info['Config']['Env']
    assert not any('must-not-be-inherited' in e or token in e for e in info['Config']['Env'])
    request('/servers/'+sid+'/stop',{},'POST')
    stopped=update(True);assert stopped['status']=='succeeded',stopped
    final=cli('ps','-a','-q','game');assert final!=after
    assert not inspect(final)['State']['Running']
    assert cli('ps','-q','dependency')==dep
    # Every source change must invalidate registration before any update.
    compose.write_text(compose.read_text()+'\n# changed owner source\n')
    cap=request('/servers/'+sid+'/update-capability')['capability'];assert not cap['available'],cap
    print(json.dumps({'result':'pass','logicalIdentitySurvived':True,'alreadyCurrent':unchanged['status'],'forcedRunning':forced['status'],'forcedStopped':stopped['status'],'dependencyUntouched':True,'literalDollarPreserved':True,'sourceChangeRejected':True}))
finally:
    subprocess.run(['docker','rm','-fv',app],capture_output=True)
    subprocess.run(['docker','compose','--project-name',project,'-f',str(compose),'down','--volumes','--remove-orphans'],capture_output=True)
    import shutil;shutil.rmtree(root)
