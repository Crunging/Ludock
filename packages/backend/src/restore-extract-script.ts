/** Private restore transport. The backend validates the tar and sends bounded
 * JSON records followed by exact file bytes; no archive command interprets paths. */
export const RESTORE_EXTRACT_SCRIPT = String.raw`
const fs=require("node:fs/promises"),C=require("node:fs").constants;
const request=JSON.parse(process.argv[1]);
const flags=C.O_RDONLY|C.O_DIRECTORY|C.O_NOFOLLOW;
const owned=new Set();
const pin=async(name,options)=>{const fd=await fs.open(name,options);owned.add(fd);return fd;};
const close=async(fd)=>{owned.delete(fd);await fd.close();};
const link=(fd,name)=>"/proc/self/fd/"+fd.fd+(name?"/"+name:"");
const parts=(name)=>{if(typeof name!=="string"||name.includes("\0")||name.includes("\\")||name.split("/").some(p=>p===".."||p==="."||p==="")||Buffer.byteLength(name)>4096)throw new Error();return name.split("/");};
(async()=>{
 if(!request.root.startsWith("/")||!/^\.ludock-restore-[0-9a-f-]{36}$/.test(request.stage))throw new Error();
 let root=await pin("/",flags);
 for(const part of parts(request.root.slice(1))){const next=await pin(link(root,part),flags);await close(root);root=next;}
 const stage=await pin(link(root,request.stage),flags);const destination=await pin(link(stage,"new"),flags);
 const parent=async(names)=>{let current=destination;for(const name of names){try{await fs.mkdir(link(current,name),{mode:0o700});}catch(e){if(e.code!=="EEXIST")throw e;}
 const next=await pin(link(current,name),flags);await current.sync();if(current!==destination)await close(current);current=next;}return current;};
 const metadata=async(fd,record)=>{await fd.chown(record.uid,record.gid);await fd.chmod(record.mode&0o777);if(Number.isFinite(record.mtime))await fd.utimes(record.mtime,record.mtime);};
 const directories=[];let buffer=Buffer.alloc(0),current=null,count=0,total=0;
 const finish=async()=>{if(!current)return;await metadata(current.file,current.record);await current.file.sync();await close(current.file);current=null;};
 for await(const chunk of process.stdin){buffer=Buffer.concat([buffer,chunk]);
  while(buffer.length){
   if(current){const take=Math.min(current.remaining,buffer.length);let offset=0;while(offset<take){const result=await current.file.write(buffer,offset,take-offset);offset+=result.bytesWritten;}
    buffer=buffer.subarray(take);current.remaining-=take;if(current.remaining===0)await finish();continue;}
   const end=buffer.indexOf(10);if(end===-1){if(buffer.length>16384)throw new Error();break;}
   if(end>16384)throw new Error();const record=JSON.parse(buffer.subarray(0,end).toString());buffer=buffer.subarray(end+1);
   if(++count>100000||!["file","directory"].includes(record.type)||!Number.isSafeInteger(record.size)||record.size<0||!Number.isSafeInteger(record.uid)||record.uid<0||record.uid>4294967294||!Number.isSafeInteger(record.gid)||record.gid<0||record.gid>4294967294||!Number.isSafeInteger(record.mode))throw new Error();
   total+=record.size;if(total>request.maxBytes)throw new Error();const names=parts(record.name);if(names.length>64||names.some(name=>name.startsWith(".ludock-restore-")))throw new Error();
   const owner=await parent(names.slice(0,-1));const name=names.at(-1);
   if(record.type==="directory"){if(record.size!==0)throw new Error();try{await fs.mkdir(link(owner,name),{mode:0o700});}catch(e){if(e.code!=="EEXIST")throw e;}const directory=await pin(link(owner,name),flags);await close(directory);directories.push(record);}
   else{const file=await pin(link(owner,name),C.O_WRONLY|C.O_CREAT|C.O_EXCL|C.O_NOFOLLOW);current={file,record,remaining:record.size};if(record.size===0)await finish();}
   await owner.sync();if(owner!==destination)await close(owner);
  }
 }
 if(current||buffer.length)throw new Error();
 for(const record of directories.reverse()){const names=parts(record.name),owner=await parent(names.slice(0,-1)),directory=await pin(link(owner,names.at(-1)),flags);await metadata(directory,record);await directory.sync();await close(directory);if(owner!==destination)await close(owner);}
 await destination.sync();process.stdout.write("ok");
})().catch(()=>{process.stderr.write("Restore extraction failed: the archive or destination changed or is unsafe.");process.exitCode=1;}).finally(async()=>{await Promise.all([...owned].map(file=>file.close().catch(()=>{})));});
`;
