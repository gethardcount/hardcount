import sys
sec, text = sys.argv[1], sys.argv[2]
p='CHANGELOG.md'; s=open(p).read()
u=s.index('## [Unreleased]'); nxt=s.index('\n## [', u+5)
h=s.find('### '+sec+'\n', u)
if h==-1 or h>nxt:
    # add section right after Unreleased heading
    ins=u+len('## [Unreleased]\n')
    s=s[:ins]+'\n### '+sec+'\n\n- '+text+'\n'+s[ins:]
else:
    ins=h+len('### '+sec+'\n\n')
    s=s[:ins]+'- '+text+'\n'+s[ins:]
open(p,'w').write(s)
